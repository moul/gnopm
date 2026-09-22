package gnopm

import (
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

// Why answers "who still imports this version", which is the question `bump`
// raises and the one `tidy` answers silently.
//
// Before dropping a pinned version you want to know what would stop resolving.
// The alternative is grep, and grep gets it wrong in both directions: it misses
// the materialized assembly, where a superseded version importing an older one
// is exactly what keeps that older one alive, and it matches any quoted path,
// including a realm naming itself.
func Why(e *Env, target string) error {
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	module, err := findLockedModule(lock, target)
	if err != nil {
		return err
	}
	graph, err := importGraph(e.Root)
	if err != nil {
		return err
	}
	users := importers(graph, module)

	if e.JSON {
		if users == nil {
			users = []string{}
		}
		return e.writeJSON(map[string]any{"module": module, "importers": users})
	}
	if e.Format != "" {
		// One record per importer, not one record holding a list: -f is a
		// line-per-record shape, so a template over a slice would make every
		// caller write a range action to get the obvious thing.
		vals := make([]any, len(users))
		for i, u := range users {
			vals[i] = WhyRecord{Importer: u, Module: module}
		}
		return e.emit(vals...)
	}
	if len(users) == 0 {
		// Nothing on stdout: an empty answer has to pipe as empty. The
		// explanation is a diagnostic and goes where diagnostics go.
		e.logf("nothing in this workspace imports %s\n", module)
		if en, ok, _ := findModule(lock, module); ok && !en.Source.InTree() {
			e.logf("  it is pinned to history. `gnopm tidy` drops a pinned version nothing\n" +
				"  imports, once its commit is also absent from the default branch.\n")
		}
		return nil
	}
	if e.Quiet {
		for _, u := range users {
			e.printf("%s\n", u)
		}
		return nil
	}
	w := tabwriter.NewWriter(e.Out, 0, 0, 1, ' ', 0)
	for _, u := range users {
		fmt.Fprintf(w, "%s\timports\t%s\n", u, module)
	}
	return w.Flush()
}

func cmdWhy(e *Env, fs *flag.FlagSet, args []string) error {
	if len(args) == 0 {
		// Standing in a package is an answer, so asking for one would be
		// asking a question gnopm can work out.
		pkg, err := packageAtCwd(e.Root)
		if err != nil {
			return err
		}
		args = []string{pkg}
	}
	if len(args) > 1 {
		return fmt.Errorf("why takes one module, got %d", len(args))
	}
	return Why(e, args[0])
}

// findLockedModule resolves a module path, a directory, or any unambiguous
// part of one against the lock.
//
// Against the lock rather than against the working tree, which is what
// separates this from findTarget: the interesting question is usually about a
// version that no longer has a directory, and that version is in the lock and
// nowhere else.
func findLockedModule(lock *Lock, target string) (string, error) {
	t := strings.TrimSuffix(filepath.ToSlash(strings.TrimSpace(target)), "/")
	if t == "" {
		return "", fmt.Errorf("no module given")
	}
	for _, en := range lock.Modules {
		if en.Module == t {
			return en.Module, nil
		}
	}
	var cands []string
	for _, en := range sortedModules(lock) {
		if en.Source.Dir == t || strings.HasSuffix(en.Module, "/"+t) || strings.Contains(en.Module, "/"+t+"/") {
			cands = append(cands, en.Module)
		}
	}
	switch len(cands) {
	case 1:
		return cands[0], nil
	case 0:
		return "", fmt.Errorf("no module matching %q in %s\n  `gnopm ls -q` lists them", target, lockFile)
	}
	// A directory holds every version of its package, so naming one is
	// ambiguous by construction the moment a version has been bumped. Name the
	// candidates rather than guessing at the latest: the older one is usually
	// the one being asked about.
	return "", fmt.Errorf("%q is ambiguous: %s", target, strings.Join(cands, ", "))
}
