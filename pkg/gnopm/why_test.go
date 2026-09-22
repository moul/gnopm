package gnopm

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestWhy covers the whole point of the command: the answer has to include
// importers that live only in the materialized assembly.
//
// grep over the working tree gets that case wrong, and so would a graph built
// from scanPackages alone: after a bump the superseded version has no directory
// at all, and it is exactly the version most likely to be holding an older one
// alive.
func TestWhy(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n\nfunc A() string { return \"a\" }\n")
	addPkg(t, root, "r/b", "gno.land/r/b/v0", "package b\n\nimport \"gno.land/p/a/v0\"\n\nfunc B() string { return a.A() }\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	// Bump r/b, so r/b/v0 exists only under .gnopm and is an assembly-side
	// importer of p/a/v0 while r/b/v1 is a tree-side one.
	mustRun(t, root, "bump", "r/b")

	out := mustRun(t, root, "why", "gno.land/p/a/v0")
	for _, want := range []string{"gno.land/r/b/v0", "gno.land/r/b/v1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("why did not name %s:\n%s", want, out)
		}
	}

	// -q is the piping shape: importer paths only, nothing else.
	var qout, qerr bytes.Buffer
	if err := Run([]string{"why", "-C", root, "-q", "gno.land/p/a/v0"}, &qout, &qerr); err != nil {
		t.Fatalf("why -q: %v\n%s", err, qerr.String())
	}
	if got := qout.String(); got != "gno.land/r/b/v0\ngno.land/r/b/v1\n" {
		t.Fatalf("why -q printed %q", got)
	}

	// Nothing imports the newest version, and that answer has to pipe as
	// empty rather than as a sentence.
	var nout, nerr bytes.Buffer
	if err := Run([]string{"why", "-C", root, "gno.land/r/b/v1"}, &nout, &nerr); err != nil {
		t.Fatalf("why on an unimported module: %v", err)
	}
	if nout.String() != "" {
		t.Fatalf("an empty answer wrote to stdout: %q", nout.String())
	}
	if !strings.Contains(nerr.String(), "nothing in this workspace imports") {
		t.Fatalf("no explanation on stderr:\n%s", nerr.String())
	}
}

// TestWhyResolvesTargets covers the argument, which is the part a user gets
// wrong: a directory holds every version of its package, so naming one is
// ambiguous the moment a version has been bumped.
func TestWhyResolvesTargets(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	addPkg(t, root, "r/b", "gno.land/r/b/v0", "package b\n\nimport \"gno.land/p/a/v0\"\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	mustRun(t, root, "bump", "p/a")

	// An unambiguous fragment resolves.
	if out := mustRun(t, root, "why", "p/a/v0"); !strings.Contains(out, "gno.land/r/b/v0") {
		t.Fatalf("a module suffix did not resolve:\n%s", out)
	}
	for _, tc := range []struct{ name, arg, want string }{
		{"ambiguous directory", "p/a", "is ambiguous"},
		{"unknown", "nope", "no module matching"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			err := Run([]string{"why", "-C", root, tc.arg}, &out, &errw)
			if err == nil {
				t.Fatalf("expected an error mentioning %q, got none", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestImportGraphOwnership pins the attribution rule: a file under a
// subdirectory belongs to the package above it, and a package naming itself is
// not a dependency.
//
// The scan this replaced read each file's OWN directory for a gnomod.toml, so
// a filetests/ file was attributed to nothing and could not be told apart from
// a package importing itself. Nothing caught it because tidy only ever looked
// at the set of imported paths, never at who imported them.
func TestImportGraphOwnership(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	addPkg(t, root, "r/b", "gno.land/r/b/v0", "package b\n")
	write(t, filepath.Join(root, "r/b/filetests/x.gno"),
		"package main\n\nimport (\n\t\"gno.land/p/a/v0\"\n\t\"gno.land/r/b/v0\"\n)\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	g, err := importGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(g["gno.land/r/b/v0"], ",")
	if got != "gno.land/p/a/v0" {
		t.Fatalf("r/b/v0 imports %q, want just gno.land/p/a/v0 (the filetest belongs to it, and its self-import does not count)", got)
	}
}
