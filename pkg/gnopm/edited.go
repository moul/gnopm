package gnopm

import (
	"fmt"
	"sort"
	"strings"
)

// Editing a published version in place is the one mistake the lock cannot see.
//
// `bump -if-published` exists so an author never has to know by heart whether a
// version is on a chain. Nothing made them run it, and in September 2026 a
// repository shipped a rendering fix straight into a realm whose version was
// live: a public package path cannot be redeployed, so the tree's v0 and the
// chain's v0 diverged permanently and no check said a word. The lock was
// consistent throughout, every pin reproduced, and nothing survived a squash
// merge wrongly. This file is the check that was missing.
//
// It warns rather than fails. A repository can have a good reason to edit a
// published version, a comment, a doc string, a test helper that happens to
// live in a prod file, and turning that into a red build teaches people to skip
// the check. Naming it is enough, because the fix is one command.

// editedInPlace returns the packages whose PRODUCTION source this branch
// changes without moving their module line.
//
// Production source only: a _test.gno or _filetest.gno never reaches a chain,
// so changing one cannot make the tree disagree with what is deployed. Neither
// can a README or the gnomod.toml itself, and a package the branch adds has no
// published version to contradict.
func editedInPlace(root string, pkgs []Package, base string) ([]Package, error) {
	// Three dots: what this branch changed, not what the base moved on to
	// underneath it. Two dots would name every package someone else touched.
	out, err := git(root, "diff", "--name-only", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	var touched []string
	for _, f := range strings.Split(out, "\n") {
		if f = strings.TrimSpace(f); isProdGno(f) {
			touched = append(touched, f)
		}
	}
	if len(touched) == 0 {
		return nil, nil
	}

	var edited []Package
	for _, p := range pkgs {
		if p.Ignored || !anyFileUnder(touched, p.Dir) {
			continue
		}
		was, err := moduleAt(root, base, p.Dir)
		if err != nil {
			// No gnomod.toml at base means the branch adds this package, which
			// is the one case where there is nothing to have bumped.
			continue
		}
		if was != p.Module {
			continue // already bumped, which is the whole point
		}
		edited = append(edited, p)
	}
	sort.Slice(edited, func(i, j int) bool { return edited[i].Module < edited[j].Module })
	return edited, nil
}

// isProdGno reports whether a repository path is gno source that gets
// deployed. It mirrors the chain's own split, which excludes test files from
// the stored package.
func isProdGno(f string) bool {
	return strings.HasSuffix(f, ".gno") &&
		!strings.HasSuffix(f, "_test.gno") &&
		!strings.HasSuffix(f, "_filetest.gno")
}

// anyFileUnder reports whether any path sits directly inside dir. Directly,
// not recursively: p/moul/kit and p/moul/kit/ui are two packages, and a change
// to the second is not a change to the first.
func anyFileUnder(files []string, dir string) bool {
	for _, f := range files {
		if strings.HasPrefix(f, dir+"/") && !strings.Contains(f[len(dir)+1:], "/") {
			return true
		}
	}
	return false
}

// moduleAt reads a package's declared module path as of a git ref.
func moduleAt(root, ref, dir string) (string, error) {
	b, err := git(root, "show", ref+":"+dir+"/gnomod.toml")
	if err != nil {
		return "", err
	}
	module, _, err := parseGnomod([]byte(b), ref+":"+dir+"/gnomod.toml")
	return module, err
}

// publishedEdits filters editedInPlace down to the versions a chain actually
// has, and describes what it found.
//
// published is the question asked of the chain, injected so the report path is
// testable without a network: CI passes a Probe, tests pass an answer.
func publishedEdits(edited []Package, published func(module string) (bool, error)) ([]Package, error) {
	var out []Package
	for _, p := range edited {
		yes, err := published(p.Module)
		if err != nil {
			return nil, err
		}
		if yes {
			out = append(out, p)
		}
	}
	return out, nil
}

// editedWarning is the sentence the report carries. It names the command,
// because an author who has to look one up will instead do nothing.
func editedWarning(pkgs []Package) string {
	names := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		names = append(names, "`"+p.Module+"`")
	}
	return fmt.Sprintf("%s is published and cannot be redeployed, so this edit can never reach a chain. `gnopm bump -if-published %s`",
		strings.Join(names, ", "), pkgs[0].Dir)
}
