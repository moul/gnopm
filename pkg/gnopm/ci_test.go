package gnopm

import (
	"bytes"
	"path/filepath"
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
	// And the two checks that did pass still say so, so the reader can see
	// the failure is narrow.
	if strings.Count(out.String(), "✅") != 2 {
		t.Errorf("expected the other two checks to pass:\n%s", out.String())
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
