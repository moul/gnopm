package gnopm

import (
	"fmt"
	"path/filepath"
	"sort"
)

// Unbump lowers a package to the lowest version number nothing has taken.
//
// The inverse of bump, for the bump that should not have happened. A version
// number is a tag on something published; the numbers between the last
// published version and the one in the working tree are tags on nothing. They
// name no bytes, resolve for no importer, and derive no address. Whatever the
// tree holds will be published as the next number after the last real one, and
// every number above that is a hole somebody has to explain later.
//
// A stack of pull requests produces those holes by default. The first bumps v0
// to v1 and lands; the second is written against it, sees v1 taken, and goes
// to v2. Both merge before either is published, and the package arrives on
// chain as v2 with v1 existing nowhere. Worse, the second bump often cannot
// even pin v1, because v1 lives on no commit the default branch has, so the
// lock skips the number entirely and the hole is not visible locally at all.
//
// Unbump leaves the files exactly where they are and moves the module line
// down, so the work lands inside the version that had not shipped yet and the
// diff a reviewer reads is still the change itself.
//
// The target is read off the chain, not off the lock: the lowest number above
// every published version of this path. That is what makes the operation safe
// rather than merely guarded. It cannot withdraw a published version, because
// a published version is not above itself, and it cannot redefine one either,
// because the target is by construction a number the chain has never seen.
func Unbump(e *Env, target string, opts UnbumpOptions) error {
	root, w := e.Root, e.Errw
	pkgs, err := scanPackages(root)
	if err != nil {
		return err
	}
	pkg, err := findTarget(pkgs, target)
	if err != nil {
		return err
	}
	base, cur, ok := splitVersion(pkg.Module)
	if !ok {
		return fmt.Errorf("module %q has no trailing /vN, nothing to unbump", pkg.Module)
	}
	if cur == 0 {
		return fmt.Errorf("%s is already at v0; there is no number below it", pkg.Module)
	}
	lock, err := readLock(root)
	if err != nil {
		return err
	}

	want, chainID, err := unbumpTarget(e, lock, pkg, base, cur, opts)
	if err != nil {
		return err
	}
	if want >= cur {
		e.logf("%s is already the lowest free version; nothing to unbump\n", pkg.Module)
		return nil
	}
	wantModule := fmt.Sprintf("%s/v%d", base, want)

	// Every number from the target up to and including the current one is
	// about to stop existing. A version something still imports cannot go: the
	// import would resolve to nothing the moment its entry does.
	imports, err := workspaceImports(root)
	if err != nil {
		return err
	}
	var discard []string
	for n := want; n <= cur; n++ {
		m := fmt.Sprintf("%s/v%d", base, n)
		if _, held, _ := findModule(lock, m); !held {
			continue
		}
		if imports[m] {
			return fmt.Errorf("something in this workspace still imports %s.\n"+
				"  Folding it away would leave that import unresolvable. Repoint it at %s first.\n"+
				"  `gnopm ls -q` lists what resolves today", m, wantModule)
		}
		discard = append(discard, m)
	}
	// The target may be held by another package's directory, which would make
	// two directories claim one module path.
	if en, held, _ := findModule(lock, wantModule); held && en.Source.InTree() && en.Source.Dir != pkg.Dir {
		return fmt.Errorf("%s is already in the working tree at %s.\n"+
			"  Two directories cannot both be %s. Sort that out first", wantModule, en.Source.Dir, wantModule)
	}

	// Rewrite the entries explicitly rather than rewriting the module line and
	// re-locking. buildLock carries over any entry whose module is not in the
	// tree, so a plain relock would keep the abandoned version as a { dir }
	// entry pointing at a directory that no longer declares it, two modules
	// would claim one directory, and nothing would say so.
	gone := map[string]bool{}
	for _, m := range discard {
		gone[m] = true
	}
	out := &Lock{Format: lockFormat}
	for _, en := range lock.Modules {
		if !gone[en.Module] {
			out.Modules = append(out.Modules, en)
		}
	}
	out.Modules = append(out.Modules, LockEntry{Module: wantModule, Source: Source{Dir: pkg.Dir}})
	out.Sort()

	gnomod := filepath.Join(root, filepath.FromSlash(pkg.Dir), "gnomod.toml")
	if err := setModuleLine(gnomod, wantModule); err != nil {
		return err
	}
	if err := writeLock(root, out); err != nil {
		return err
	}

	fmt.Fprintf(w, "unbumped %s\n  %s -> %s\n  %s: one-line module change, files unchanged\n",
		pkg.Dir, pkg.Module, wantModule, filepath.Join(pkg.Dir, "gnomod.toml"))
	if n := len(discard) - 1; n > 0 {
		fmt.Fprintf(w, "  %s: %d superseded entr%s dropped\n", lockFile, n, plural(n, "y", "ies"))
	}
	if chainID != "" {
		fmt.Fprintf(w, "\nv%d is the lowest number %s has never seen, so nothing it stood for is lost.\n", want, chainID)
	}
	fmt.Fprintf(w, "The change now lands inside v%d instead of behind a v%d nobody would ever have seen.\n", want, cur)
	return nil
}

// UnbumpOptions is what unbump was asked to do.
type UnbumpOptions struct {
	// Force skips the chain entirely and falls back to the previous version
	// the lock happens to hold, for an unreachable chain and an author who is
	// certain. It is strictly worse: the lock cannot see a number that was
	// skipped rather than pinned, which is the common case.
	Force bool
	// RPC and ChainID skip chain discovery, for a local gnodev.
	RPC, ChainID string
}

// unbumpTarget works out which version the package should be at.
func unbumpTarget(e *Env, lock *Lock, pkg Package, base string, cur int, opts UnbumpOptions) (int, string, error) {
	if opts.Force {
		_, prev, ok := previousVersion(lock, base, cur)
		if !ok {
			return 0, "", fmt.Errorf("%s has no earlier version in %s to fall back to.\n"+
				"  Without the chain, that is all -force has to go on. Drop -force to ask the chain",
				pkg.Module, lockFile)
		}
		e.logf("-force: falling back to v%d, the previous version in %s, without asking a chain\n", prev, lockFile)
		return prev, "", nil
	}
	probe, err := NewProbe(e, pkg.Module, opts.RPC, opts.ChainID)
	if err != nil {
		return 0, "", fmt.Errorf("%w\n  unbump reads the chain to find the lowest version nothing has taken.\n"+
			"  -force falls back to the previous version in "+lockFile, err)
	}
	state, err := probe.State(pkg.Module)
	if err != nil {
		return 0, "", err
	}
	if state != StateAbsent {
		return 0, "", fmt.Errorf("%s is %s on %s: a published version cannot be withdrawn.\n"+
			"  Something already resolves that path, so the number is spent. Leave it",
			pkg.Module, state, probe.Chain().ID)
	}
	want, err := FirstFreeVersion(probe, base, cur)
	if err != nil {
		return 0, "", err
	}
	return want, probe.Chain().ID, nil
}

// previousVersion finds the highest version of base below n that the lock
// knows about. Only -force uses it: the lock cannot see a number that was
// skipped rather than pinned.
func previousVersion(l *Lock, base string, n int) (module string, version int, ok bool) {
	var found []int
	for _, en := range l.Modules {
		b, v, isVersioned := splitVersion(en.Module)
		if isVersioned && b == base && v < n {
			found = append(found, v)
		}
	}
	if len(found) == 0 {
		return "", 0, false
	}
	sort.Ints(found)
	best := found[len(found)-1]
	return fmt.Sprintf("%s/v%d", base, best), best, true
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
