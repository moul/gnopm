package gnopm

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitMakesAWorkspaceFromNothing is the path a newcomer actually walks, and
// the one nothing covered: every other test here starts from newRepo, which
// writes gnowork.toml itself and so can never see that a human had no way to.
func TestInitMakesAWorkspaceFromNothing(t *testing.T) {
	dir := t.TempDir()

	var out, errb bytes.Buffer
	if err := Run([]string{"init", "-C", dir}, &out, &errb); err != nil {
		t.Fatalf("init: %v\n%s", err, errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, workspaceMarker)); err != nil {
		t.Fatalf("no %s: %v", workspaceMarker, err)
	}
	// It has to say what to do next. A command that leaves you at a prompt
	// with a new empty directory has moved the dead end, not removed it.
	if got := errb.String(); !strings.Contains(got, "gno mod init") || !strings.Contains(got, "gnopm status") {
		t.Errorf("init did not say what comes next:\n%s", got)
	}

	// init deliberately does not create a repository, and says so, because the
	// lock pins to commits and bump will need one.
	if !strings.Contains(errb.String(), "no git repository") {
		t.Errorf("init did not mention that git is still needed:\n%s", errb.String())
	}

	// And the workspace has to actually work from there.
	gitCmd(t, dir, "init", "-q", "-b", "main")
	gitCmd(t, dir, "config", "user.email", "demo@example.com")
	gitCmd(t, dir, "config", "user.name", "gnopm test")
	addPkg(t, dir, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, dir, "first package")
	mustRun(t, dir, "sync")
	if s := mustRun(t, dir, "status"); !strings.Contains(s, "up to date") {
		t.Fatalf("the workspace init made does not settle:\n%s", s)
	}
	mustRun(t, dir, "verify")
}

// TestInitIsIdempotentAndRefusesNesting. Running it twice is what somebody does
// when they are not sure it worked; running it inside an existing workspace is
// the mistake worth refusing, because nested workspaces resolve ambiguously and
// nothing about the directory you are standing in shows the nesting.
func TestInitIsIdempotentAndRefusesNesting(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	if err := Run([]string{"init", "-C", dir}, &out, &errb); err != nil {
		t.Fatal(err)
	}
	before := read(t, filepath.Join(dir, workspaceMarker))

	errb.Reset()
	if err := Run([]string{"init", "-C", dir}, &out, &errb); err != nil {
		t.Fatalf("a second init is not an error: %v", err)
	}
	if !strings.Contains(errb.String(), "already") {
		t.Errorf("a second init said nothing useful:\n%s", errb.String())
	}
	if read(t, filepath.Join(dir, workspaceMarker)) != before {
		t.Error("a second init rewrote the marker")
	}

	nested := filepath.Join(dir, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	err := Run([]string{"init", "-C", nested}, &out, &errb)
	if err == nil {
		t.Fatal("init nested a workspace inside a workspace")
	}
	if !strings.Contains(err.Error(), "already inside the workspace") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// TestNotAWorkspaceSaysHowToMakeOne. This error is the first thing a newcomer
// meets, and it used to name gnowork.toml without saying it was theirs to
// create, which is a dead end dressed as a diagnosis.
func TestNotAWorkspaceSaysHowToMakeOne(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	err := Run([]string{"status", "-C", dir}, &out, &errb)
	if err == nil {
		t.Fatal("status succeeded outside a workspace")
	}
	if !strings.Contains(err.Error(), "gnopm init") {
		t.Errorf("the error does not offer a way out:\n%v", err)
	}
}

// TestBorrowedVerbsGetAReason covers the table by behaviour rather than by
// content: what matters is that the words people arrive typing produce an
// explanation instead of "unknown command", and that the wrong-but-confident
// edit-distance guess no longer fires for them.
func TestBorrowedVerbsGetAReason(t *testing.T) {
	dir := t.TempDir()
	for _, word := range []string{"install", "add", "i", "update", "remove", "test", "audit"} {
		var out, errb bytes.Buffer
		err := Run([]string{word, "-C", dir}, &out, &errb)
		if err == nil {
			t.Errorf("%s: expected an error", word)
			continue
		}
		if strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%s: still a shrug: %v", word, err)
		}
		if strings.Contains(err.Error(), "Did you mean") {
			t.Errorf("%s: edit distance guessed instead of explaining: %v", word, err)
		}
	}
}
