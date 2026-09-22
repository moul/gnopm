package gnopm

import (
	"bytes"
	"strings"
	"testing"
)

// conflictedRepo reproduces the merge that motivated the command: two branches
// that each bumped a different package, so each carries a pin the other has no
// entry for at all.
//
// The inserted packages (p/ab on one side, p/ac on the other) sort next to each
// other, which is what makes git's own merge of gnomod.lock conflict rather
// than quietly succeed. That is the real shape: a lock is sorted, so two
// branches adding packages near each other collide.
func conflictedRepo(t *testing.T) string {
	t.Helper()
	root := newRepo(t)
	for _, n := range []string{"a", "b", "c"} {
		addPkg(t, root, "p/"+n, "gno.land/p/"+n+"/v0", "package "+n+"\n")
	}
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	commit(t, root, "lock")

	gitCmd(t, root, "checkout", "-q", "-b", "feature")
	mustRun(t, root, "bump", "p/a")
	addPkg(t, root, "p/ab", "gno.land/p/ab/v0", "package ab\n")
	mustRun(t, root, "sync")
	commit(t, root, "feature bumps a")

	gitCmd(t, root, "checkout", "-q", "main")
	mustRun(t, root, "bump", "p/b")
	addPkg(t, root, "p/ac", "gno.land/p/ac/v0", "package ac\n")
	mustRun(t, root, "sync")
	commit(t, root, "main bumps b")

	// Expected to fail. That is the situation under test.
	if _, err := git(root, "merge", "feature"); err == nil {
		t.Fatal("the merge did not conflict, so this test proves nothing")
	}
	if out, _ := git(root, "ls-files", "-u", "--", lockFile); strings.TrimSpace(out) == "" {
		t.Fatal("gnomod.lock merged cleanly, so this test proves nothing")
	}
	return root
}

// TestMergeLock is the whole issue, end to end: the union keeps both pins,
// where taking either side would have dropped one silently.
func TestMergeLock(t *testing.T) {
	root := conflictedRepo(t)

	// First, prove the failure this exists to prevent. Our side of the merge
	// has no entry at all for the version the other side pinned, so --ours
	// loses it, sync cannot restore it (buildLock only carries over entries
	// the old lock already had), and verify still passes on the poorer lock.
	ours, ok := stageLock(root, 2)
	if !ok {
		t.Fatal("no stage 2")
	}
	if _, found, _ := findModule(ours, "gno.land/p/a/v1"); found {
		t.Fatal("our side already has the other branch's bump, so --ours would lose nothing")
	}

	out := mustRun(t, root, "merge-lock")
	for _, want := range []string{
		"take theirs gno.land/p/a/v0 (pinned there",
		"take ours   gno.land/p/b/v0 (pinned here",
		"resolved and staged",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the report does not contain %q:\n%s", want, out)
		}
	}

	lock := mustLock(t, root)
	byModule, err := lock.ByModule()
	if err != nil {
		t.Fatal(err)
	}
	// Both pins, from both sides. Losing either is the bug.
	for _, m := range []string{"gno.land/p/a/v0", "gno.land/p/b/v0"} {
		en := byModule[m]
		if en == nil {
			t.Fatalf("%s was lost in the resolution", m)
		}
		if en.Source.InTree() || en.Source.Commit == "" || en.Hash == "" {
			t.Fatalf("%s is no longer pinned: %+v", m, en.Source)
		}
	}
	// And both sides' new packages.
	for _, m := range []string{"gno.land/p/a/v1", "gno.land/p/b/v1", "gno.land/p/ab/v0", "gno.land/p/ac/v0", "gno.land/p/c/v0"} {
		if byModule[m] == nil {
			t.Fatalf("%s was lost in the resolution", m)
		}
	}

	// No markers left, staged, and the tool's own guard passes on the result.
	// That last one is the test case the issue asked for by name.
	if strings.Contains(read(t, root+"/"+lockFile), "<<<<<<<") {
		t.Fatal("conflict markers survived")
	}
	if staged, _ := git(root, "diff", "--name-only", "--cached"); !strings.Contains(staged, lockFile) {
		t.Fatalf("the lock was not staged: %q", staged)
	}
	if err := VerifyWith(testEnv(root, new(bytes.Buffer)), ""); err != nil {
		t.Fatalf("verify fails on the resolved lock: %v", err)
	}
}

// TestMergeLockDryRun: nobody should first meet this command mid-merge with no
// way to look before it writes.
func TestMergeLockDryRun(t *testing.T) {
	root := conflictedRepo(t)
	before := read(t, root+"/"+lockFile)

	out := mustRun(t, root, "merge-lock", "-n")
	if !strings.Contains(out, "take theirs gno.land/p/a/v0") {
		t.Fatalf("-n did not print the resolution:\n%s", out)
	}
	if !strings.Contains(out, "nothing written") {
		t.Fatalf("-n did not say it wrote nothing:\n%s", out)
	}
	if read(t, root+"/"+lockFile) != before {
		t.Fatal("-n rewrote the lock")
	}
	if out, _ := git(root, "ls-files", "-u", "--", lockFile); strings.TrimSpace(out) == "" {
		t.Fatal("-n resolved the conflict in the index")
	}
}

// TestMergeLockRefusesAmbiguous covers the one case the rule cannot decide.
func TestMergeLockRefusesAmbiguous(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/c", "gno.land/p/c/v0", "package c\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	commit(t, root, "lock")

	// Both branches edit the same package and then bump it, so each pins the
	// outgoing version to its own commit. Not what a squash merge produces,
	// and guessing would strand whichever version is real.
	for _, side := range []string{"sideA", "sideB"} {
		gitCmd(t, root, "checkout", "-q", "main")
		gitCmd(t, root, "checkout", "-q", "-b", side)
		write(t, root+"/p/c/c.gno", "package c // "+side+"\n")
		commit(t, root, "edit on "+side)
		mustRun(t, root, "bump", "p/c")
		commit(t, root, "bump on "+side)
	}
	if _, err := git(root, "merge", "sideA"); err == nil {
		t.Fatal("the merge did not conflict")
	}

	var out, errw bytes.Buffer
	err := Run([]string{"merge-lock", "-C", root}, &out, &errw)
	if err == nil {
		t.Fatal("two different pins for one module were resolved silently")
	}
	for _, want := range []string{"pinned to different commits", "gno.land/p/c/v0", "by hand"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestMergeLockNoConflict: running it out of habit after a merge that only
// touched source files has to cost nothing.
func TestMergeLockNoConflict(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	commit(t, root, "lock")

	var out, errw bytes.Buffer
	if err := Run([]string{"merge-lock", "-C", root}, &out, &errw); err != nil {
		t.Fatalf("merge-lock outside a conflict is not an error: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("it wrote to stdout: %q", out.String())
	}
	if !strings.Contains(errw.String(), "not conflicted") {
		t.Fatalf("it did not say why it did nothing: %q", errw.String())
	}
}
