package gnopm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinnedRepo is a workspace where one version LEFT the working tree.
//
// base is bumped to v1, so the directory p/moul/base now declares
// gno.land/p/moul/base/v1 and gno.land/p/moul/base/v0 exists only in this
// repository's git history, pinned by gnomod.lock and materialized under
// .gnopm/ by sync. app still imports v0, which is the whole reason the pin is
// there: a live importer needs the version it imports to be live too.
func pinnedRepo(t *testing.T) string {
	t.Helper()
	root := newRepo(t)
	addPkg(t, root, "p/moul/base", "gno.land/p/moul/base/v0",
		"package base\n\nfunc B() string { return \"b\" }\n")
	addPkg(t, root, "r/moul/app", "gno.land/r/moul/app/v0",
		"package app\n\nimport \"gno.land/p/moul/base/v0\"\n\nfunc A() string { return base.B() }\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	mustRun(t, root, "bump", "p/moul/base")
	commit(t, root, "bump base to v1")
	mustRun(t, root, "sync")

	if en := mustByModule(t, root)["gno.land/p/moul/base/v0"]; en == nil || en.Source.Variant() != "commit" {
		t.Fatalf("fixture did not pin base/v0 to history: %+v", en)
	}
	if !dirExists(assemblyPathOf(root, "gno.land/p/moul/base/v0")) {
		t.Fatal("fixture did not materialize base/v0")
	}
	return root
}

// TestPublishPublishesAVersionPinnedToHistory.
//
// What went wrong: publish built its package set from the lock with
// `if !m.Source.InTree() { continue }`, so a version pinned to this
// repository's own history was not merely left out of the plan, it was invisible
// as a dependency too. Its importer was then judged against the chain alone and
// reported "not live, and not in this workspace" for a package whose bytes were
// sitting in .gnopm/, pinned by commit and hash in gnomod.lock. On
// moul/gno-contracts that blocked five packages behind four pinned versions,
// and the diagnosis sent a reader looking for a version bump nothing needed
// (moul/gno-meta#827).
//
// Why no existing test could see it: every publish fixture was a flat working
// tree. No publish test had ever run `bump`, so no publish test had ever had a
// version leave the tree, which is the only way a { commit, dir } entry is
// created.
func TestPublishPublishesAVersionPinnedToHistory(t *testing.T) {
	root := pinnedRepo(t)
	f := newFakeChain(t)

	script, report, err := publishPlan(t, root, f)
	if err != nil {
		t.Fatalf("publish refused a workspace whose only off-tree version is its own history: %v\n%s", err, report)
	}
	if strings.Contains(report, "not in this workspace") {
		t.Fatalf("a version pinned by gnomod.lock was called absent from the workspace:\n%s", report)
	}
	if strings.Contains(report, "blocked by") {
		t.Fatalf("the importer is blocked by a dependency this run can publish:\n%s", report)
	}
	for _, m := range []string{"gno.land/p/moul/base/v0", "gno.land/p/moul/base/v1", "gno.land/r/moul/app/v0"} {
		if !strings.Contains(script, "-pkgpath "+m+" ") {
			t.Fatalf("%s is not in the script:\n%s\n--- report ---\n%s", m, script, report)
		}
	}
	// Ordered like any other dependency: the pinned version goes up before the
	// package that imports it, or the second transaction fails.
	if v0, app := strings.Index(script, "-pkgpath gno.land/p/moul/base/v0"),
		strings.Index(script, "-pkgpath gno.land/r/moul/app/v0"); v0 > app {
		t.Fatalf("pinned dependency is published after its importer:\n%s", script)
	}
	// And the bytes come from the assembly, which is the only place they are.
	if !strings.Contains(script, filepath.Join(root, assemblyDir, "gno.land/p/moul/base/v0")) {
		t.Fatalf("script does not read the pinned version from the assembly:\n%s", script)
	}
	if !strings.Contains(report, "(pinned to history)") {
		t.Errorf("report does not say the bytes have no directory of their own:\n%s", report)
	}
}

// TestPublishProvesAPinnedVersionAgainstTheLock.
//
// The assembly is generated, gitignored and rebuildable, so it is exactly the
// kind of input a publish must not take on faith. gnomod.lock carries a hash
// for every pinned version; a copy that does not match it is not published, and
// the importer is blocked by a reason naming the command that fixes it rather
// than by the generic "not in this workspace".
func TestPublishProvesAPinnedVersionAgainstTheLock(t *testing.T) {
	root := pinnedRepo(t)
	tampered := filepath.Join(assemblyPathOf(root, "gno.land/p/moul/base/v0"), "base.gno")
	if err := os.WriteFile(tampered, []byte("package base\n\nfunc B() string { return \"evil\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFakeChain(t)

	script, report, err := publishPlan(t, root, f)
	if err == nil {
		t.Fatalf("publish uploaded an assembly copy that does not match gnomod.lock:\n%s\n%s", script, report)
	}
	if strings.Contains(script, "-pkgpath gno.land/p/moul/base/v0") {
		t.Fatalf("the tampered copy went into the script:\n%s", script)
	}
	if !strings.Contains(report, "does not match "+lockFile) || !strings.Contains(report, "gnopm sync") {
		t.Fatalf("report does not say what is wrong or how to fix it:\n%s", report)
	}
}

// TestPublishStampsThePinnedCommitNotHead: a pinned version's [source]
// revision must be the commit the lock extracted it from, and its path the
// directory it lived in then.
//
// HEAD is the wrong answer twice over. The directory p/moul/base now holds v1,
// so HEAD's copy of that path is a DIFFERENT package, and the dir publish reads
// from is .gnopm/..., which is gitignored and empty in a fresh clone. A
// repository URL on chain pointing at either is a lie no redeploy can take back.
func TestPublishStampsThePinnedCommitNotHead(t *testing.T) {
	root := pinnedRepo(t)
	en := mustByModule(t, root)["gno.land/p/moul/base/v0"]
	head, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	head = strings.TrimSpace(head)
	if en.Source.Commit == head {
		t.Fatal("fixture pins base/v0 at HEAD, so this test cannot tell the two apart")
	}

	r := newSourceResolver(testEnv(root, os.Stderr), false)
	r.isPublic = func(string) (bool, error) { return true, nil }
	gitCmd(t, root, "remote", "add", "origin", "https://github.com/moul/contracts")

	pl := &plan{pkg: Package{Dir: assemblyRelOf("gno.land/p/moul/base/v0"), Module: "gno.land/p/moul/base/v0"}}
	pin := en.Source
	if err := pl.stampSource(root, r, &pin); err != nil {
		t.Fatal(err)
	}
	_, s, found, err := splitSource(pl.gnomod)
	if err != nil || !found {
		t.Fatalf("no [source] was stamped (found=%v): %v\n%s", found, err, pl.gnomod)
	}
	if s.Revision != en.Source.Commit {
		t.Errorf("revision is %q, want the pinned commit %q", s.Revision, en.Source.Commit)
	}
	if s.Path != "p/moul/base" {
		t.Errorf("path is %q, want the directory the pinned version lived in", s.Path)
	}
	if strings.Contains(s.Path, assemblyDir) {
		t.Errorf("path points at the gitignored assembly: %q", s.Path)
	}
}
