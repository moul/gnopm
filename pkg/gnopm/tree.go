package gnopm

import (
	"flag"
	"sort"
)

// The dependency tree, drawn in the terminal.
//
// `gnopm graph` already computes this and emits DOT, which is the right default
// for a file you pipe into graphviz or commit to CI. It is the wrong shape for
// the question people actually ask, which is "what does this pull in", typed
// once, read once, in a terminal that has no graphviz. `cargo tree` and
// `npm ls` are the same command and everybody arrives knowing them.
//
// So this is not a second graph implementation: it reads the same importGraph
// and prints it differently.

// treeGlyphs are the box-drawing characters, and their ASCII fallback.
//
// -ascii rather than sniffing the locale: a terminal that lies about UTF-8 is
// common, the failure is ugly rather than fatal, and a flag can be put in a
// script where an autodetection cannot be argued with.
type treeGlyphs struct{ mid, last, bar, gap string }

var (
	unicodeGlyphs = treeGlyphs{"├── ", "└── ", "│   ", "    "}
	asciiGlyphs   = treeGlyphs{"|-- ", "`-- ", "|   ", "    "}
)

// Tree prints the import graph rooted at every package nothing else imports,
// or at the packages named.
//
// Roots, not every package, because printing every node as a root prints the
// whole workspace once per level and the shape stops being legible at about
// twenty packages. A workspace where everything imports something still has
// roots: the realms.
func Tree(e *Env, targets []string, ascii, showAll bool) error {
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	graph, err := importGraph(e.Root)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, en := range lock.Modules {
		known[en.Module] = true
	}

	var roots []string
	if len(targets) > 0 {
		for _, t := range targets {
			m, err := findLockedModule(lock, t)
			if err != nil {
				return err
			}
			roots = append(roots, m)
		}
	} else {
		imported := map[string]bool{}
		for _, to := range graph {
			for _, t := range to {
				imported[t] = true
			}
		}
		for m := range known {
			if !imported[m] {
				roots = append(roots, m)
			}
		}
		// A cycle would leave every node imported and so produce no roots at
		// all. gno import paths cannot cycle across packages, but a lock
		// assembled by hand can say anything, and printing nothing is the
		// least useful way to report it.
		if len(roots) == 0 {
			for m := range known {
				roots = append(roots, m)
			}
			e.logf("nothing here is unimported, so every module is shown as a root\n")
		}
		sort.Strings(roots)
	}

	g := unicodeGlyphs
	if ascii {
		g = asciiGlyphs
	}
	elided := false
	for _, r := range roots {
		e.printf("%s\n", r)
		seen := map[string]bool{r: true}
		walkTree(e, graph, known, r, "", g, seen, showAll, &elided)
	}
	if elided {
		// Data on stdout and nothing else, so the tree still pipes.
		e.logf("(*) already shown under this root; -all expands every repeat\n")
	}
	return nil
}

// walkTree prints one node's imports.
//
// seen is per-root rather than global: a package imported by two different
// roots is interesting under both, and eliding the second is how `npm ls`
// output becomes a thing people stop trusting. Within one root it is elided,
// because there the repetition is the same subtree twice.
func walkTree(e *Env, graph map[string][]string, known map[string]bool, node, prefix string, g treeGlyphs, seen map[string]bool, showAll bool, elided *bool) {
	deps := append([]string(nil), graph[node]...)
	sort.Strings(deps)
	// A dependency outside the lock is one the workspace imports and cannot
	// resolve. Hiding it would make the tree disagree with `gnopm publish`,
	// which calls exactly that a blocker.
	for i, d := range deps {
		lastOne := i == len(deps)-1
		branch, next := g.mid, g.bar
		if lastOne {
			branch, next = g.last, g.gap
		}
		label := d
		switch {
		case !known[d]:
			// "not in this workspace" is true of the lock and false of the
			// disk when a repository keeps a hand-made vendor/ tree, which
			// both gno-contracts and the demo do. Saying so sends people
			// looking for a directory that is right in front of them.
			if vendorExists(e.Root, d) {
				label += "  (vendored, not in the lock)"
				break
			}
			label += "  (not in this workspace)"
		case seen[d] && !showAll:
			// Say it was repeated rather than printing nothing, or the tree
			// silently understates what a package depends on.
			e.printf("%s%s%s  (*)\n", prefix, branch, label)
			*elided = true
			continue
		}
		e.printf("%s%s%s\n", prefix, branch, label)
		if !known[d] {
			continue
		}
		seen[d] = true
		walkTree(e, graph, known, d, prefix+next, g, seen, showAll, elided)
	}
}

func cmdTree(e *Env, fs *flag.FlagSet, args []string) error {
	return Tree(e, args, flagBool(fs, "ascii"), flagBool(fs, "all"))
}
