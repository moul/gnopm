package gnopm

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// treeRepo builds a workspace with a shape worth drawing: a realm importing
// two packages, one of which imports a third, plus one import that resolves
// nowhere.
func treeRepo(t *testing.T) string {
	t.Helper()
	root := newRepo(t)
	addPkg(t, root, "p/leaf", "gno.land/p/leaf/v0", "package leaf\n\nfunc L() string { return \"l\" }\n")
	addPkg(t, root, "p/mid", "gno.land/p/mid/v0",
		"package mid\n\nimport \"gno.land/p/leaf/v0\"\n\nfunc M() string { return leaf.L() }\n")
	addPkg(t, root, "r/app", "gno.land/r/app/v0",
		"package app\n\nimport (\n\t\"gno.land/p/mid/v0\"\n\t\"gno.land/p/leaf/v0\"\n\t\"gno.land/p/nowhere/v0\"\n)\n\nfunc A() string { return mid.M() + leaf.L() + nowhere.N() }\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	return root
}

// TestTreeDrawsTheGraph covers the shape, the roots, and the two labels.
//
// The unresolvable import is the assertion that matters: hiding it would make
// the tree disagree with `gnopm publish`, which calls exactly that a blocker,
// and a tree that quietly omits what breaks your deploy is worse than none.
func TestTreeDrawsTheGraph(t *testing.T) {
	root := treeRepo(t)
	out := mustRun(t, root, "tree")

	// Only the realm is a root: nothing imports it, everything else is
	// imported. Printing every package as a root prints the workspace once
	// per level.
	if !strings.HasPrefix(out, "gno.land/r/app/v0\n") {
		t.Fatalf("the unimported package is not the root:\n%s", out)
	}
	for _, m := range []string{"gno.land/p/mid/v0", "gno.land/p/leaf/v0"} {
		if !strings.Contains(out, m) {
			t.Errorf("%s is missing:\n%s", m, out)
		}
	}
	if !strings.Contains(out, "gno.land/p/nowhere/v0") ||
		!strings.Contains(out, "not in this workspace") {
		t.Errorf("an unresolvable import was hidden:\n%s", out)
	}
	if !strings.Contains(out, "└──") && !strings.Contains(out, "├──") {
		t.Errorf("nothing was drawn as a tree:\n%s", out)
	}
}

// TestTreeAsciiAndRepeats: -ascii for a terminal that lies about its encoding,
// and a repeated subtree marked rather than silently dropped, because a tree
// that understates what a package depends on is one people stop trusting.
func TestTreeAsciiAndRepeats(t *testing.T) {
	root := treeRepo(t)

	var out, errb bytes.Buffer
	if err := Run([]string{"tree", "-C", root, "-ascii"}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "├└│") {
		t.Errorf("-ascii still drew box characters:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "`--") && !strings.Contains(out.String(), "|--") {
		t.Errorf("-ascii drew no tree at all:\n%s", out.String())
	}

	// leaf is imported by the realm directly and by mid, so under this root it
	// appears twice and the second is marked.
	if !strings.Contains(out.String(), "(*)") {
		t.Errorf("a repeated subtree was not marked:\n%s", out.String())
	}
	if !strings.Contains(errb.String(), "(*)") {
		t.Errorf("nothing explained the marker:\n%s", errb.String())
	}

	// -all expands it instead, and the explanation goes away with it.
	var aout, aerr bytes.Buffer
	if err := Run([]string{"tree", "-C", root, "-all"}, &aout, &aerr); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(aout.String(), "(*)") {
		t.Errorf("-all still elided a subtree:\n%s", aout.String())
	}
}

// TestTreeNamesVendoredRatherThanMissing. "not in this workspace" is true of
// the lock and false of the disk when a repository keeps a hand-made vendor/
// tree, which both gno-contracts and gnopm-demo do, so it sent people looking
// for a directory that was right in front of them.
func TestTreeNamesVendoredRatherThanMissing(t *testing.T) {
	root := treeRepo(t)
	dir := filepath.Join(root, vendorDir, "gno.land", "p", "nowhere", "v0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "gnomod.toml"), "module = \"gno.land/p/nowhere/v0\"\ngno = \"0.9\"\n")
	write(t, filepath.Join(dir, "nowhere.gno"), "package nowhere\n\nfunc N() string { return \"n\" }\n")

	out := mustRun(t, root, "tree")
	if !strings.Contains(out, "vendored") {
		t.Errorf("a vendored package was still reported as absent:\n%s", out)
	}
}

// TestTreeOnANamedPackage: the argument is resolved against the lock the same
// way why and doc resolve theirs, so a version with no directory can be a root.
func TestTreeOnANamedPackage(t *testing.T) {
	root := treeRepo(t)
	out := mustRun(t, root, "tree", "gno.land/p/mid/v0")
	if !strings.HasPrefix(out, "gno.land/p/mid/v0\n") {
		t.Fatalf("the named package is not the root:\n%s", out)
	}
	if strings.Contains(out, "gno.land/r/app/v0") {
		t.Errorf("the tree climbed upwards:\n%s", out)
	}
}
