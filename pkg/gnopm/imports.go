package gnopm

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// importGraph returns every import edge gnopm can see, keyed by the importing
// module path.
//
// Both halves of the workspace count. The working tree holds the versions
// being edited, and the materialized assembly holds the superseded ones: a
// superseded version importing an older one is exactly how a chain of versions
// stays alive, so a graph built from the tree alone would report a pinned
// version as unused and invite dropping it.
//
// Values are the domain-prefixed paths those files import, deduplicated and
// sorted, with a module's own path excluded. A package naming itself is not a
// dependency.
func importGraph(root string) (map[string][]string, error) {
	set := map[string]map[string]bool{}
	add := func(from, to string) {
		if from == "" || from == to {
			return
		}
		if set[from] == nil {
			set[from] = map[string]bool{}
		}
		set[from][to] = true
	}
	pkgs, err := scanPackages(root)
	if err != nil {
		return nil, err
	}
	for _, p := range pkgs {
		if err := walkImports(filepath.Join(root, filepath.FromSlash(p.Dir)), add); err != nil {
			return nil, err
		}
	}
	if err := walkImports(filepath.Join(root, assemblyDir), add); err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(set))
	for from, tos := range set {
		list := make([]string, 0, len(tos))
		for to := range tos {
			list = append(list, to)
		}
		sort.Strings(list)
		out[from] = list
	}
	return out, nil
}

// walkImports walks base and reports every gno.land import it finds, attributed
// to the module that owns the file.
//
// Ownership is the nearest enclosing gnomod.toml, tracked as the walk descends,
// so a file under filetests/ belongs to the package above it while a nested
// package keeps its own imports. Reading each file's own directory instead,
// which is what this replaced, left every filetest attributed to nothing and so
// unable to exclude a package naming itself.
func walkImports(base string, add func(from, to string)) error {
	owner := map[string]string{}
	return filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // a missing assembly is not an error here
		}
		if d.IsDir() {
			name := d.Name()
			if p != base && (strings.HasPrefix(name, ".") || skipDirs[name]) {
				return filepath.SkipDir
			}
			parent := ""
			if p != base {
				parent = owner[filepath.Dir(p)]
			}
			owner[p] = parent
			if mod, _, err := readGnomod(filepath.Join(p, "gnomod.toml")); err == nil {
				owner[p] = mod
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".gno") {
			return nil
		}
		from := owner[filepath.Dir(p)]
		if from == "" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, imp := range importsOf(b) {
			add(from, imp)
		}
		return nil
	})
}

// importers returns the modules that import target, sorted.
func importers(graph map[string][]string, target string) []string {
	var out []string
	for from, tos := range graph {
		for _, to := range tos {
			if to == target {
				out = append(out, from)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
