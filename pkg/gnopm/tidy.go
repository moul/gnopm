package gnopm

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// importsOf returns the gno.land paths a file actually imports.
//
// Parsed from the import declarations, not matched anywhere in the file. A
// naive search for quoted gno.land strings also catches things like
// chain.PackageAddress("gno.land/r/moul/x/amm/v0"), a realm naming itself,
// which made tidy believe every package imported itself and therefore that
// nothing was ever droppable.
func importsOf(src []byte) []string {
	var out []string
	s := string(src)
	for _, m := range importBlockRe.FindAllStringSubmatch(s, -1) {
		for _, q := range quotedRe.FindAllStringSubmatch(m[1], -1) {
			if strings.HasPrefix(q[1], "gno.land/") {
				out = append(out, q[1])
			}
		}
	}
	for _, m := range importLineRe.FindAllStringSubmatch(s, -1) {
		if strings.HasPrefix(m[1], "gno.land/") {
			out = append(out, m[1])
		}
	}
	return out
}

var (
	importBlockRe = regexp.MustCompile(`(?s)\bimport\s*\((.*?)\)`)
	importLineRe  = regexp.MustCompile(`(?m)^\s*import\s+(?:[\w.]+\s+)?"([^"]+)"`)
	quotedRe      = regexp.MustCompile(`"([^"]+)"`)
)

// workspaceImports returns every gno.land path imported by anything gnopm can
// see: the working tree and the materialized assembly both count, because a
// superseded version importing an older one is exactly how a chain of versions
// stays alive.
func workspaceImports(root string) (map[string]bool, error) {
	out := map[string]bool{}
	scan := func(dir string) error {
		return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // a missing assembly is not an error here
			}
			if d.IsDir() {
				if p != dir && (strings.HasPrefix(d.Name(), ".") && d.Name() != assemblyDir) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".gno") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			self := ""
			if mod, _, err := readGnomod(filepath.Join(filepath.Dir(p), "gnomod.toml")); err == nil {
				self = mod
			}
			for _, imp := range importsOf(b) {
				if imp == self {
					continue // a package naming itself is not a dependency
				}
				out[imp] = true
			}
			return nil
		})
	}
	pkgs, err := scanPackages(root)
	if err != nil {
		return nil, err
	}
	for _, pk := range pkgs {
		if err := scan(filepath.Join(root, filepath.FromSlash(pk.Dir))); err != nil {
			return nil, err
		}
	}
	if err := scan(filepath.Join(root, assemblyDir)); err != nil {
		return nil, err
	}
	return out, nil
}

// Tidy is the heavy one: the command you run when you want the workspace
// right, not the one a Makefile prerequisite calls.
//
// `sync` is the cheap, silent, idempotent maintenance command, and it stays
// that way. Tidy is allowed to cost git walks and chain reads, in the spirit
// of `go mod tidy`: bring the lock in line, prove every pinned version still
// reproduces out of history, drop what is provably dead, and then ask the
// chain the one question no local check can answer, which is whether any of
// these version numbers ever meant anything.
//
// It writes only what is safe to write without asking. The chain pass reports
// and names the command; folding a version back down changes a package's
// identity and is `gnopm unbump`, run deliberately, one package at a time.
func Tidy(e *Env, opts TidyOptions) error {
	w := e.Errw
	if opts.DryRun {
		lock, err := readLock(e.Root)
		if err != nil {
			return err
		}
		pkgs, err := scanPackages(e.Root)
		if err != nil {
			return err
		}
		if err := consistent(lock, pkgs); err != nil {
			fmt.Fprintf(w, "lock         stale: %v (sync would fix it)\n", err)
		} else {
			fmt.Fprintf(w, "lock         up to date\n")
		}
	} else if err := Sync(e); err != nil {
		return err
	}

	// Drop before proving, not after. verify's upstream check rejects a pin to
	// a commit that is not on the default branch, and dropping exactly those
	// pins is what the next pass does, so proving first made tidy fail with
	// the error its own third pass exists to remove.
	if err := tidyLock(e, opts.DryRun); err != nil {
		return err
	}

	// The expensive proof, which is most of why tidy is not sync: re-read
	// every pinned version out of git and re-hash it, so a rewritten or
	// garbage-collected commit is caught here rather than by whoever's build
	// stops working next month. Last, so it proves the tidied lock rather
	// than the one tidy was handed.
	if err := VerifyWith(e, ""); err != nil {
		return err
	}
	if opts.Offline {
		fmt.Fprintf(w, "chain        skipped (-offline)\n")
		return nil
	}
	return tidyChain(e, opts)
}

// TidyOptions is what tidy was asked to do.
type TidyOptions struct {
	// DryRun prints what would change and changes nothing.
	DryRun bool
	// Offline skips the chain pass, leaving only the git-reachability proxy.
	Offline bool
	// RPC and ChainID skip chain discovery, for a local gnodev.
	RPC, ChainID string
}

// tidyLock drops pinned versions that nothing imports and that never shipped.
//
// Both conditions, not either. A version nobody in this workspace imports may
// still be deployed and imported by somebody else's code, so "unused here" is
// not permission to forget it. The second condition is what makes it safe: if
// the commit a version is pinned to is not reachable from the default branch,
// that version never existed for anyone outside the branch that created it.
// Dropping it loses nothing and un-strands a pin that a squash merge would
// have discarded anyway.
//
// That combination is the normal end state of a branch that adds v0 and then
// bumps to v1 before either has landed: the intermediate version is an editing
// artefact, not a release.
func tidyLock(e *Env, dryRun bool) error {
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	imports, err := workspaceImports(e.Root)
	if err != nil {
		return err
	}
	upstream := upstreamRef(e.Root, "")

	var drop []LockEntry
	for _, en := range materializedEntries(lock) {
		if imports[en.Module] {
			continue
		}
		if upstream != "" && gitIsAncestor(e.Root, en.Source.Commit, upstream) {
			continue // it shipped; not ours to forget
		}
		drop = append(drop, en)
	}
	if len(drop) == 0 {
		fmt.Fprintln(e.Errw, "pins         nothing to drop")
		return nil
	}
	sort.Slice(drop, func(i, j int) bool { return drop[i].Module < drop[j].Module })
	for _, en := range drop {
		why := "nothing imports it"
		if upstream != "" {
			why += ", and it never reached " + upstream
		}
		fmt.Fprintf(e.Errw, "drop %s (%s)\n", en.Module, why)
	}
	if dryRun {
		return nil
	}
	next := &Lock{Format: lockFormat}
	dropped := map[string]bool{}
	for _, en := range drop {
		dropped[en.Module] = true
	}
	for _, en := range lock.Modules {
		if !dropped[en.Module] {
			next.Modules = append(next.Modules, en)
		}
	}
	if err := writeLock(e.Root, next); err != nil {
		return err
	}
	return Install(e)
}

// tidyChain reports version numbers spent on nothing.
//
// The local passes can only use "did this commit reach the default branch" as
// a proxy for "could anyone else be depending on this", and that proxy is
// wrong in the direction that costs a number: a version can sit on the default
// branch for months and still have been published to nobody. Merging a stack
// of pull requests is how it happens, and the result is a package that arrives
// on chain as v2 with v1 existing nowhere.
//
// So this pass asks the chain, which is the only thing that knows. It reports
// and does not act: folding a version away changes a package's identity, and
// that is `gnopm unbump`, run deliberately.
func tidyChain(e *Env, opts TidyOptions) error {
	w := e.Errw
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	var tree []LockEntry
	for _, en := range sortedModules(lock) {
		if en.Source.InTree() {
			tree = append(tree, en)
		}
	}
	if len(tree) == 0 {
		fmt.Fprintf(w, "chain        nothing in the working tree to check\n")
		return nil
	}
	probe, err := NewProbe(tree[0].Module, opts.RPC, opts.ChainID)
	if err != nil {
		return fmt.Errorf("%w\n  `gnopm tidy -offline` skips the chain pass", err)
	}
	fmt.Fprintf(w, "chain        %s (%s)\n", probe.Chain().ID, probe.Chain().RPC)

	live, parked, absent := 0, 0, 0
	var spent []string
	for _, en := range tree {
		state, err := probe.State(en.Module)
		if err != nil {
			return err
		}
		switch state {
		case StateLive:
			live++
			continue
		case StateParked:
			parked++
			continue
		}
		absent++
		// This version is absent, so its number is still a claim rather than a
		// fact. The claim is only honest if it is the next one after the last
		// published version; anything higher names bytes nobody will ever be
		// able to fetch under that number.
		base, cur, ok := splitVersion(en.Module)
		if !ok || cur == 0 {
			continue
		}
		want, err := FirstFreeVersion(probe, base, cur)
		if err != nil {
			return err
		}
		if want >= cur {
			continue
		}
		gap := ""
		if cur-want > 1 {
			gap = fmt.Sprintf("v%d through v%d are", want, cur-1)
		} else {
			gap = fmt.Sprintf("v%d is", want)
		}
		spent = append(spent, fmt.Sprintf("  %s\n    %s free: nothing published has ever taken %s.\n"+
			"    gnopm unbump %s lowers it to v%d.",
			en.Module, gap, plural(cur-want, "that number", "those numbers"), en.Source.Dir, want))
	}
	fmt.Fprintf(w, "             %d live, %d parked, %d absent\n", live, parked, absent)
	if !probe.CanPark() {
		fmt.Fprintf(w, "             this chain cannot park a submission, so absent really is absent\n")
	}
	// Silent when there is nothing to report: every version number here is
	// either on the chain or sits directly above one that is.
	if len(spent) == 0 {
		return nil
	}
	fmt.Fprintf(w, "\n%d version number(s) spent on nothing:\n", len(spent))
	for _, l := range spent {
		fmt.Fprintln(w, l)
	}
	return nil
}
