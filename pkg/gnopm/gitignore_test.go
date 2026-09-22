package gnopm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAssemblyIgnoresItself: gnopm creates .gnopm/, so gnopm is the one that
// has to make sure nothing commits it.
//
// A committed assembly reintroduces exactly the duplicated-source problem the
// tool exists to remove. Before this, every adopting repository had to know to
// write the rule, and the only code on the subject was a function nothing
// called.
func TestAssemblyIgnoresItself(t *testing.T) {
	root := newRepoNoIgnore(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, root, "initial")

	out := mustRun(t, root, "sync")
	if !strings.Contains(out, "added /"+assemblyDir+"/") {
		t.Fatalf("sync did not say it wrote the rule:\n%s", out)
	}
	ignore := read(t, filepath.Join(root, ".gitignore"))
	if !strings.Contains(ignore, "/"+assemblyDir+"/") {
		t.Fatalf(".gitignore is %q", ignore)
	}
	// The point of the exercise: git must not offer it.
	status, err := git(root, "status", "--short")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(status, assemblyDir) {
		t.Fatalf("git still sees the assembly:\n%s", status)
	}
}

// TestAssemblyIgnoreIsIdempotent is the regression test for the bug that made
// this worth writing carefully.
//
// /.gnopm/ is a directory-only pattern, and `git check-ignore .gnopm` on a path
// that does not exist yet cannot know it is a directory, so it answers "not
// ignored" and the rule gets appended again on every run. That is precisely the
// state this code runs in: before anything has been written into the assembly.
// Asking about ".gnopm/" instead tells git it is a directory either way.
//
// The first version of this test passed, because it ran a second sync with the
// directory still on disk. Removing it between runs is what exposes it.
func TestAssemblyIgnoreIsIdempotent(t *testing.T) {
	root := newRepoNoIgnore(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	first := read(t, filepath.Join(root, ".gitignore"))
	for i := 0; i < 3; i++ {
		// Remove the directory, which is what makes the naive check wrong.
		if err := os.RemoveAll(filepath.Join(root, assemblyDir)); err != nil {
			t.Fatal(err)
		}
		mustRun(t, root, "sync")
		if got := read(t, filepath.Join(root, ".gitignore")); got != first {
			t.Fatalf("run %d rewrote .gitignore:\n got %q\nwant %q", i+2, got, first)
		}
	}
}

// TestAssemblyIgnoreRespectsWhatIsAlreadyThere: the rule can be spelled half a
// dozen ways and can live somewhere other than .gitignore, so the question has
// to go to git rather than to a string compare.
func TestAssemblyIgnoreRespectsWhatIsAlreadyThere(t *testing.T) {
	t.Run("a differently spelled rule", func(t *testing.T) {
		root := newRepoNoIgnore(t)
		write(t, filepath.Join(root, ".gitignore"), assemblyDir+"/\n")
		addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
		commit(t, root, "initial")
		mustRun(t, root, "sync")
		if got := read(t, filepath.Join(root, ".gitignore")); got != assemblyDir+"/\n" {
			t.Fatalf("an existing rule was not honoured: %q", got)
		}
	})

	t.Run("git/info/exclude", func(t *testing.T) {
		root := newRepoNoIgnore(t)
		write(t, filepath.Join(root, ".git", "info", "exclude"), assemblyDir+"/\n")
		addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
		commit(t, root, "initial")
		mustRun(t, root, "sync")
		if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
			t.Fatalf("a .gitignore was written although the path was already excluded: %v", err)
		}
	})

	t.Run("not a git repository", func(t *testing.T) {
		// A workspace outside git is a legitimate thing (a tarball, a
		// container build), and there is nothing to ignore it with, so this
		// has to be silent rather than an error.
		root := t.TempDir()
		write(t, filepath.Join(root, workspaceMarker), "")
		addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
		mustRun(t, root, "sync")
		if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
			t.Fatalf("a .gitignore was written outside a git repository: %v", err)
		}
	})
}

// TestAssemblyIgnoreAppends: an existing .gitignore is added to, not replaced,
// and the file keeps a trailing newline whether or not it had one.
func TestAssemblyIgnoreAppends(t *testing.T) {
	root := newRepoNoIgnore(t)
	// No trailing newline, which is the case that concatenates two rules onto
	// one line if the append is naive.
	write(t, filepath.Join(root, ".gitignore"), "*.log\nbuild")
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	got := read(t, filepath.Join(root, ".gitignore"))
	if !strings.HasPrefix(got, "*.log\nbuild\n") {
		t.Fatalf("the existing rules were damaged: %q", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.TrimSpace(line) == "build" {
			return
		}
	}
	t.Fatalf("the existing rules were lost: %q", got)
}

// newRepoNoIgnore is newRepo without the .gitignore it writes, because that is
// the thing under test here.
func newRepoNoIgnore(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, workspaceMarker), "")
	gitCmd(t, root, "init", "-q", "-b", "main")
	gitCmd(t, root, "config", "user.email", "t@example.com")
	gitCmd(t, root, "config", "user.name", "t")
	gitCmd(t, root, "config", "commit.gpgsign", "false")
	return root
}
