package gnopm

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	// State is what the chain says about the path at all.
	State PackageState
	// Match is whether the deployed bytes equal the committed ones. Only
	// meaningful when State is StateLive.
	Match bool
	// Tree and Chain are the two hashes, for the report when they differ.
	Tree, Chain string
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

	bar := newProgress(e, "reading "+chain.ID)
	err = probe.Warm(tree, bar.step)
	bar.stop()
	if err != nil {
		return err
	}

	var results []deployedResult
	for _, m := range tree {
		st, err := probe.State(m)
		if err != nil {
			return err
		}
		r := deployedResult{Module: m, State: st}
		if st == StateLive {
			// The source, not the state. This is the query the whole command
			// exists for and the only one that costs real bytes, so it runs
			// only for what is actually live.
			got, err := fetchPackage(chain, m)
			if err != nil {
				return fmt.Errorf("reading %s back from %s: %w", m, chain.ID, err)
			}
			want, err := hashPayload(filepath.Join(e.Root, filepath.FromSlash(dirOf[m])))
			if err != nil {
				return err
			}
			r.Chain, r.Tree = got.Hash, want
			r.Match = got.Hash == want
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
			e.logf("differs  %s\n           on %s: %s\n           in the tree: %s\n",
				r.Module, chain.ID, r.Chain, r.Tree)
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
