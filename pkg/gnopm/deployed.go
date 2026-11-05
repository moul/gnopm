package gnopm

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Proving that what is deployed is what is committed.
//
// Every other check here is about this repository being internally consistent:
// the lock describes the tree, a pinned version reproduces from history, a
// downloaded dependency hashes to what the lock recorded. None of them can see
// the one thing that actually matters once a package ships, which is whether
// the bytes running on a chain are the bytes in this commit.
//
// Nothing else can answer it either. A chain cannot be asked "did this repo
// produce you", and a repository cannot be asked "are you live". The only way
// is to read the deployed source back and hash both sides, which is what this
// does, and it is possible only because a published path cannot be redefined:
// the answer is stable, so a mismatch is real rather than a race.
//
// It is the last bullet of roadmap section 2 in #2, and the reason gnopm is
// more than a package manager: a build can be reproducible and still be
// deploying something nobody reviewed.

// deployedResult is one package's verdict.
type deployedResult struct {
	Module string
	// Path is where the package actually is on this chain, which is Module
	// unless a [[deployment]] row says otherwise. See deployedPaths.
	Path string
	// State is what the chain says about the path at all.
	State PackageState
	// Match is whether the deployed bytes equal the committed ones. Only
	// meaningful when State is StateLive.
	Match bool
	// Changed names the files that differ, which is the whole content of a
	// "no": a hash pair says that something moved and never what.
	Changed []string
}

// hashPayload hashes exactly what `addpkg -pkgdir <dir>` would upload.
//
// Not the git-tracked set that hashFiles documents, and the difference is the
// whole point: what is on chain is what publish sent, so comparing against
// anything else compares two things that were never meant to be equal.
//
// The two sets really do differ, in both directions. A Makefile or a .png is
// tracked and not uploaded, because uploadable() takes only .gno, .toml, .md
// and a few licence names; a filetest under filetests/ is uploaded and lives in
// a subdirectory git walks past. README.md is NOT an example of the first kind,
// which is what the first draft of this comment claimed and a test disproved:
// .md is uploaded, and gno.land renders it.
func hashPayload(dir string) (string, error) {
	files, err := payloadFiles(dir)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("%s uploads no files", dir)
	}
	byName := make(map[string]string, len(files))
	names := make([]string, 0, len(files))
	for _, f := range files {
		byName[f.Name] = f.Path
		names = append(names, f.Name)
	}
	return hashFiles(names, func(name string) (io.ReadCloser, error) {
		return os.Open(byName[name])
	})
}

// VerifyDeployed reads every live in-tree package back off the chain and
// compares it with the working tree.
//
// Reported per package rather than summarised, because "3 of 40 differ" is a
// number nobody can act on: which three is the entire content of the answer.
func VerifyDeployed(e *Env, rpc, chainID string) error {
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	pkgs, err := scanPackages(e.Root)
	if err != nil {
		return err
	}
	dirOf := make(map[string]string, len(pkgs))
	for _, p := range pkgs {
		dirOf[p.Module] = p.Dir
	}
	var tree []string
	for _, en := range sortedModules(lock) {
		if _, ok := dirOf[en.Module]; ok {
			tree = append(tree, en.Module)
		}
	}
	if len(tree) == 0 {
		e.logf("nothing in the working tree to check against a chain\n")
		return nil
	}

	probe, err := NewProbe(e, tree[0], rpc, chainID)
	if err != nil {
		return err
	}
	chain := probe.Chain()
	e.logf("chain        %s (%s)\n", chain.ID, chain.RPC)

	// Where each package actually is on THIS chain.
	//
	// It is not always the path in the tree. On a chain where the publisher
	// could not use the namespace, the upload went under their address and the
	// publish recorded the result as a [[deployment]] row (deployment.go). This
	// command holds no key, so it cannot work the mapping out for itself the
	// way publish does: the rows ARE the mapping, which is the third reason
	// they exist.
	//
	// Without this, every package rehearsed on a testnet answers "absent" here
	// forever, and every one that did resolve would compare the tree's imports
	// against the rewritten ones and read as drifted. Both are reported
	// failures for a deploy that is exactly right.
	moved := deployedPaths(e.Root, pkgs, chain.ID)
	pm := &pathMap{to: moved}
	domain := tree[0]
	if i := strings.IndexByte(domain, '/'); i >= 0 {
		domain = domain[:i]
	}
	if len(moved) > 0 {
		e.logf("paths        %d package(s) are deployed under a different path here, per gnomod.toml\n", len(moved))
	}

	want := make([]string, 0, len(tree))
	for _, m := range tree {
		want = append(want, pm.Upload(m))
	}
	bar := newProgress(e, "reading "+chain.ID)
	err = probe.Warm(want, bar.step)
	bar.stop()
	if err != nil {
		return err
	}

	var results []deployedResult
	for _, m := range tree {
		upload := pm.Upload(m)
		st, err := probe.State(upload)
		if err != nil {
			return err
		}
		r := deployedResult{Module: m, Path: upload, State: st}
		if st == StateLive {
			// The source, not the state. This is the query the whole command
			// exists for and the only one that costs real bytes, so it runs
			// only for what is actually live.
			got, err := chainSource(chain, upload)
			if err != nil {
				return fmt.Errorf("reading %s back from %s: %w", upload, chain.ID, err)
			}
			local, err := localSource(filepath.Join(e.Root, filepath.FromSlash(dirOf[m])))
			if err != nil {
				return err
			}
			for name, body := range local {
				local[name] = pm.rewriteBody(name, body, upload, domain)
			}
			// diffSource, not a pair of raw hashes. The chain REWRITES
			// gnomod.toml rather than storing what it was sent -- it drops the
			// comments and appends an [addpkg] table it authors itself -- so a
			// byte comparison of that one file reports every package as
			// differing forever, including one deployed a minute ago. See
			// gnomodnorm.go, where the same mistake was found and fixed for
			// the republish check.
			r.Changed = diffSource(local, got)
			r.Match = len(r.Changed) == 0
		}
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Module < results[j].Module })

	live, matched, differ, absent, parked := 0, 0, 0, 0, 0
	for _, r := range results {
		switch r.State {
		case StateLive:
			live++
			if r.Match {
				matched++
				e.tracef("ok       %s\n", r.Module)
				continue
			}
			differ++
			where := ""
			if r.Path != r.Module {
				where = " (deployed at " + r.Path + ")"
			}
			e.logf("differs  %s%s\n           %d file(s): %s\n",
				r.Module, where, len(r.Changed), strings.Join(r.Changed, ", "))
		case StateParked:
			parked++
		default:
			absent++
		}
	}
	e.logf("%d live, %d match, %d differ, %d parked, %d absent\n",
		live, matched, differ, parked, absent)
	if differ == 0 {
		return nil
	}
	// An error, not a warning. A package whose deployed bytes are not the
	// committed ones is the failure this command exists to find, and a CI step
	// that prints it and exits 0 finds nothing.
	return fmt.Errorf("%d package(s) deployed on %s are not what this tree holds.\n"+
		"  A published path cannot be redefined, so the chain will not catch up on its own:\n"+
		"  the fix is a new version. `gnopm bump <pkg>` then `gnopm publish`,\n"+
		"  or `gnopm doc` on each side to see what moved",
		differ, chain.ID)
}
