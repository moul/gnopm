package gnopm

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shallowFixture builds a repository with a pin, then clones it at the given
// depth and points origin/main at the clone's own HEAD.
//
// A real clone rather than a hand-written .git/shallow file: the bug is about
// what git answers, and a fixture that fakes git's state would be testing the
// fake. `file://` keeps it offline, like everything else here.
func shallowFixture(t *testing.T, depth string) (clone string) {
	t.Helper()
	src := newRepo(t)
	addPkg(t, src, "p/md", "gno.land/p/md/v0", "package md\n\nfunc Bold(s string) string { return s }\n")
	commit(t, src, "seed")
	mustRun(t, src, "sync")
	commit(t, src, "lock")
	// The bump pins v0 to the commit that still holds it, which is the commit
	// a shallow clone then cannot see.
	mustRun(t, src, "bump", "p/md")
	commit(t, src, "bump to v1")
	for _, msg := range []string{"later work", "more later work", "later still"} {
		write(t, filepath.Join(src, "NOTES.md"), msg)
		commit(t, src, msg)
	}

	clone = filepath.Join(t.TempDir(), "clone")
	args := []string{"clone", "-q"}
	if depth != "" {
		args = append(args, "--depth="+depth)
	}
	gitCmd(t, t.TempDir(), append(args, "file://"+src, clone)...)
	gitCmd(t, clone, "update-ref", "refs/remotes/origin/main", "HEAD")
	return clone
}

// TestShallowCloneIsNotAnAnswer pins moul/gnopm#72's core complaint: on a
// shallow clone the history checks reported findings instead of reporting that
// they could not look.
//
// `git merge-base --is-ancestor` returns non-zero for a commit the repository
// does not have, which is the same answer it gives for a commit that genuinely
// is not upstream. Every pin beyond the boundary therefore read as stranded.
// In moul/gno-contracts that was 39 of 42 pinned versions, under three green
// checks, with nothing wrong.
//
// The advice attached to it is what makes a wrong answer worse than no answer:
// "Bump before editing, or `gnopm tidy`", where tidy on that reading drops the
// very pins the clone could not see.
func TestShallowCloneIsNotAnAnswer(t *testing.T) {
	clone := shallowFixture(t, "1")
	if !gitIsShallow(clone) {
		t.Fatal("the fixture is not shallow, so it proves nothing")
	}

	var out, errw bytes.Buffer
	err := Run([]string{"tool", "ci", "-C", clone}, &out, &errw)
	report := out.String() + errw.String()
	if err == nil {
		t.Fatalf("a clone that cannot answer reported success:\n%s", report)
	}
	if !strings.Contains(report, "shallow clone") {
		t.Errorf("the report does not name the cause:\n%s", report)
	}
	// The fix has to be on the line that survives: the CI table keeps one line
	// per check, so advice on line two never reaches a job summary.
	for _, line := range strings.Split(report, "\n") {
		if strings.Contains(line, "shallow clone") && !strings.Contains(line, "fetch-depth: 0") {
			t.Errorf("a shallow line arrives without the fix on it:\n%s", line)
		}
	}
	// And it must not invent findings, nor recommend the command that would
	// act on them.
	if strings.Contains(report, "squash merge would lose it") {
		t.Errorf("pins it cannot see are reported as stranded:\n%s", report)
	}
	if strings.Contains(report, "gnopm tidy") {
		t.Errorf("the report advises dropping pins it could not read:\n%s", report)
	}
}

// TestShallowCloneDeepEnoughStillAnswers is the other half, and the reason the
// guard counts unreachable pins rather than just asking whether the clone is
// shallow. A shallow clone that happens to contain every pinned commit can
// answer correctly, and refusing there would be a false alarm on a repository
// doing nothing wrong.
func TestShallowCloneDeepEnoughStillAnswers(t *testing.T) {
	clone := shallowFixture(t, "50") // deeper than the history, so nothing truncates
	if gitIsShallow(clone) {
		// Shallow but complete: git only sets the boundary when it truncated.
		// If a future git starts marking this shallow, the guard must still
		// let it through, which is exactly what the assertions below check.
		t.Log("the clone is marked shallow and holds every pin, which is the case the count exists for")
	}

	var out, errw bytes.Buffer
	if err := Run([]string{"tool", "ci", "-C", clone}, &out, &errw); err != nil {
		t.Fatalf("a clone holding every pin was refused: %v\n%s", err, out.String()+errw.String())
	}
	if strings.Contains(out.String(), "shallow clone") {
		t.Errorf("a clone that could answer was told it could not:\n%s", out.String())
	}
}

// TestBeyondTheBoundaryIsQuietOnACompleteRepository: a complete repository
// missing a pinned commit is a real finding, and `reproduces` is right to
// report it as one. The guard must not swallow that.
func TestBeyondTheBoundaryIsQuietOnACompleteRepository(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/md", "gno.land/p/md/v0", "package md\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")

	// A pin to a commit nothing has, in a repository that is not shallow.
	ghost := []LockEntry{{
		Module: "gno.land/p/md/v0",
		Source: Source{Commit: strings.Repeat("a", 40), Dir: "p/md"},
	}}
	if got := beyondTheBoundary(root, ghost); got != nil {
		t.Errorf("a complete repository reported a shallow boundary: %v", got)
	}
	if err := reproduces(root, ghost); err == nil {
		t.Error("an unresolvable pin was accepted")
	} else if strings.Contains(err.Error(), "shallow") {
		t.Errorf("a real finding was reported as a shallow clone: %v", err)
	}
}

// TestBeyondTheBoundarySkipsChainEntries: a { chain } entry carries no commit,
// so it is neither reachable nor unreachable, and listing it as beyond the
// boundary would be a second wrong answer in the same place.
func TestBeyondTheBoundarySkipsChainEntries(t *testing.T) {
	clone := shallowFixture(t, "1")
	entries := []LockEntry{{Module: "gno.land/p/nt/ufmt/v0", Source: Source{Chain: "gnoland-1"}}}
	if got := beyondTheBoundary(clone, entries); got != nil {
		t.Errorf("a chain entry was reported as a missing commit: %v", got)
	}
	// Sanity: this clone really is the one that hides commits.
	if _, err := os.Stat(filepath.Join(clone, ".git", "shallow")); err != nil {
		t.Fatalf("the fixture has no shallow boundary: %v", err)
	}
}
