package gnopm

import (
	"os"
	"path/filepath"
	"testing"
)

// A pinned package keeps its filetests/, and the bug this proves is worth
// restating: sync extracted the subdirectory from history, proved the hash
// over it, and then pruned it away, so the copy could never match the lock
// again. publish then refused the importer, naming `gnopm sync` as the fix.
func TestSyncKeepsAPinnedPackageSubdirectory(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/base", "gno.land/p/moul/base/v0", "package base\n\nfunc B() string { return \"v0\" }\n")
	write(t, filepath.Join(root, "p/moul/base/filetests/z_x_filetest.gno"),
		"package main\n\nfunc main() {}\n\n// Output:\n")
	commit(t, root, "base v0")
	mustRun(t, root, "sync")
	// Bump takes v0 out of the tree and pins it to history, which is the only
	// way a { commit, dir } entry comes into being.
	mustRun(t, root, "bump", "p/moul/base")
	commit(t, root, "base v1")
	mustRun(t, root, "sync")

	ft := filepath.Join(root, assemblyDir, "gno.land/p/moul/base/v0/filetests/z_x_filetest.gno")
	if _, err := os.Stat(ft); err != nil {
		t.Fatalf("the filetest was pruned out of the assembly: %v", err)
	}

	// And the version is publishable, which is the consequence that matters:
	// the hash gnomod.lock carries is over the git file list, filetests
	// included, so a copy missing one can never prove itself.
	lock, err := readLock(root)
	if err != nil {
		t.Fatal(err)
	}
	src := publishSourcesOf(lock)
	if _, ok := src.ours(root, "gno.land/p/moul/base/v0"); !ok {
		t.Errorf("the pinned version is unpublishable: %s", src.why["gno.land/p/moul/base/v0"])
	}
}
