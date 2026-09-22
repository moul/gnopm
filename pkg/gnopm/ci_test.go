package gnopm

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestCIReportsEachCheckSeparately: "something is wrong with your lock" is not
// an actionable sentence, so the report names which check failed and why.
func TestCIReportsEachCheckSeparately(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/md", "gno.land/p/moul/md/v0", "package md\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	commit(t, root, "lock")
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")

	var out, errw bytes.Buffer
	if err := CI(&Env{Root: root, Out: &out, Errw: &errw}, CIOptions{}); err != nil {
		t.Fatalf("clean workspace should pass: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		ciMarker,
		"the lock describes the working tree",
		"pinned versions reproduce from history",
		"pins survive a squash merge",
		"1 modules", // one package, and the count is stated
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "❌") {
		t.Errorf("clean workspace reported a failure:\n%s", out.String())
	}
}

// TestCIFailsOnAStrandedPin: the failure worth having, end to end through the
// reporting path rather than only through verify.
func TestCIFailsOnAStrandedPin(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/md", "gno.land/p/moul/md/v0", "package md\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	commit(t, root, "lock")
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")

	// The mistake: edit the published version, then bump it.
	gitCmd(t, root, "checkout", "-q", "-b", "feature")
	write(t, filepath.Join(root, "p/moul/md/md.gno"), "package md // edited\n")
	commit(t, root, "edit")
	if err := Bump(&Env{Root: root, Out: &bytes.Buffer{}, Errw: &bytes.Buffer{}}, "md", BumpOptions{}); err != nil {
		t.Fatal(err)
	}
	commit(t, root, "bump")

	var out, errw bytes.Buffer
	err := CI(&Env{Root: root, Out: &out, Errw: &errw}, CIOptions{})
	if err == nil {
		t.Fatalf("CI passed with a pin a squash merge would discard:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "❌") {
		t.Errorf("the failing check is not marked in the report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "gnopm tidy") {
		t.Errorf("the report does not say how to fix it:\n%s", out.String())
	}
	// And only that check fails, so the reader can see the failure is narrow.
	// Counting failures rather than passes: the number of checks grows, and a
	// test that pinned it broke every time one was added, which is what it did.
	if got := strings.Count(out.String(), "❌"); got != 1 {
		t.Errorf("expected exactly one failing check, got %d:\n%s", got, out.String())
	}
	if strings.Contains(out.String(), "⚠️") {
		t.Errorf("a stranded pin is a failure, not a warning:\n%s", out.String())
	}
}

// TestCIReportsWhatTheBranchDidToTheLock: a lock diff is unreadable, so the
// report says what changed in words.
func TestCIReportsWhatTheBranchDidToTheLock(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/md", "gno.land/p/moul/md/v0", "package md\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	commit(t, root, "lock")
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")

	gitCmd(t, root, "checkout", "-q", "-b", "feature")
	if err := Bump(&Env{Root: root, Out: &bytes.Buffer{}, Errw: &bytes.Buffer{}}, "md", BumpOptions{}); err != nil {
		t.Fatal(err)
	}
	commit(t, root, "bump md")

	var out, errw bytes.Buffer
	if err := CI(&Env{Root: root, Out: &out, Errw: &errw}, CIOptions{}); err != nil {
		t.Fatalf("a clean bump should pass: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "bumped to **v1**") {
		t.Errorf("the bump is not described:\n%s", out.String())
	}
	// Reported once, as a bump, not twice as an add plus a pin.
	if n := strings.Count(out.String(), "gno.land/p/moul/md"); n > 2 {
		t.Errorf("the bump is reported %d times, expected once:\n%s", n, out.String())
	}
}

func TestBadgesCountWhatIsThere(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/a", "gno.land/p/moul/a/v0", "package a\n")
	addPkg(t, root, "p/moul/b", "gno.land/p/moul/b/v0", "package b\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")

	var out bytes.Buffer
	if err := Badges(&Env{Root: root, Out: &out, Errw: &bytes.Buffer{}}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "packages-2-blue") {
		t.Errorf("package count wrong:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "img.shields.io") {
		t.Errorf("not a shields badge:\n%s", out.String())
	}

	out.Reset()
	if err := Badges(&Env{Root: root, Out: &out, Errw: &bytes.Buffer{}, JSON: true}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"schemaVersion": 1`) {
		t.Errorf("-json is not the shields endpoint shape:\n%s", out.String())
	}
}

// TestBadgeEscaping: shields.io reads a single dash as a field separator, so a
// literal one has to double or the badge silently renders wrong.
func TestBadgeEscaping(t *testing.T) {
	if got := esc("up-to-date"); got != "up--to--date" {
		t.Fatalf("esc(%q) = %q", "up-to-date", got)
	}
	if got := esc("a b"); got != "a%20b" {
		t.Fatalf("esc(%q) = %q", "a b", got)
	}
}

// TestEditedInPlaceSeesOnlyWhatReachesAChain.
//
// No existing test could see any of this: every one of them drove the lock,
// and the lock is exactly what stays consistent while a published version is
// edited. That is the hole this check exists to close.
func TestEditedInPlaceSeesOnlyWhatReachesAChain(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/md", "gno.land/p/moul/md/v0", "package md\n")
	addPkg(t, root, "p/moul/kit", "gno.land/p/moul/kit/v0", "package kit\n")
	addPkg(t, root, "p/moul/tests", "gno.land/p/moul/tests/v0", "package tests\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	commit(t, root, "lock")
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitCmd(t, root, "checkout", "-q", "-b", "feature")

	// md: production source edited, module line untouched. The mistake.
	write(t, filepath.Join(root, "p/moul/md/md.gno"), "package md // edited\n")
	// kit: edited AND bumped, which is the correct shape and must not warn.
	write(t, filepath.Join(root, "p/moul/kit/kit.gno"), "package kit // edited\n")
	// tests: only a test file, which is never deployed, so it cannot diverge.
	write(t, filepath.Join(root, "p/moul/tests/tests_test.gno"), "package tests\n")
	// A package the branch adds has no published version to contradict.
	addPkg(t, root, "p/moul/new", "gno.land/p/moul/new/v0", "package new\n")
	commit(t, root, "edit")
	if err := Bump(&Env{Root: root, Out: &bytes.Buffer{}, Errw: &bytes.Buffer{}}, "kit", BumpOptions{}); err != nil {
		t.Fatal(err)
	}
	commit(t, root, "bump kit")

	pkgs, err := scanPackages(root)
	if err != nil {
		t.Fatal(err)
	}
	edited, err := editedInPlace(root, pkgs, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range edited {
		got = append(got, p.Module)
	}
	if want := []string{"gno.land/p/moul/md/v0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("editedInPlace = %v, want %v", got, want)
	}

	// And the chain decides whether it matters: an unpublished version is
	// still the version to edit, so it is not a warning.
	none, err := publishedEdits(edited, func(string) (bool, error) { return false, nil })
	if err != nil || len(none) != 0 {
		t.Errorf("unpublished edit should not warn: %v %v", none, err)
	}
	hit, err := publishedEdits(edited, func(string) (bool, error) { return true, nil })
	if err != nil || len(hit) != 1 {
		t.Fatalf("published edit should warn: %v %v", hit, err)
	}
	// The message names the command, because an author who has to look one up
	// will instead do nothing.
	if w := editedWarning(hit); !strings.Contains(w, "gnopm bump -if-published p/moul/md") {
		t.Errorf("warning does not name the fix: %s", w)
	}
}

// TestCIWarnsButDoesNotFailOnAPublishedEdit: a repository can have a good
// reason to touch a published version, so this is the one check that reports
// without turning the build red. Driven against a fake chain, because the test
// suite has no network.
func TestCIWarnsButDoesNotFailOnAPublishedEdit(t *testing.T) {
	f := newFakeChain(t)
	f.live["gno.land/p/moul/md/v0"] = true

	root := newRepo(t)
	addPkg(t, root, "p/moul/md", "gno.land/p/moul/md/v0", "package md\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	commit(t, root, "lock")
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")

	gitCmd(t, root, "checkout", "-q", "-b", "feature")
	write(t, filepath.Join(root, "p/moul/md/md.gno"), "package md // edited\n")
	commit(t, root, "edit a live version")

	var out, errw bytes.Buffer
	err := CI(&Env{Root: root, Out: &out, Errw: &errw},
		CIOptions{RPC: f.srv.URL, ChainID: "test-1"})
	if err != nil {
		t.Fatalf("a warning must not fail the run: %v\n%s", err, out.String())
	}
	for _, want := range []string{"⚠️", publishedEditCheck, "gnopm bump -if-published"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report is missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "❌") {
		t.Errorf("nothing here is a failure:\n%s", out.String())
	}
}
