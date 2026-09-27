package gnopm

import (
	"bytes"
	"strings"
	"testing"
)

// TestOutdatedFindsASuccessor is the whole command: a workspace importing v0 of
// something the chain has also published a v1 of.
//
// The comparison that makes this cheap is the point. npm and cargo need a
// registry API and a semver policy; here the question is whether a path with
// the next integer exists, and a published path can never be redefined, so the
// answer is exact rather than a best guess.
func TestOutdatedFindsASuccessor(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	addPkg(t, root, "p/b", "gno.land/p/b/v0", "package b\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	f := newFakeChain(t)
	f.live["gno.land/p/a/v1"] = true // a has moved on
	f.live["gno.land/p/a/v2"] = true
	// b has not: nothing beyond v0 exists.

	var out, errb bytes.Buffer
	if err := Run([]string{"outdated", "-C", root, "-rpc", f.srv.URL, "-chainid", "test"}, &out, &errb); err != nil {
		t.Fatalf("outdated: %v\n%s", err, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "gno.land/p/a/v0") || !strings.Contains(got, "gno.land/p/a/v2") {
		t.Errorf("did not report a's successor:\n%s", got)
	}
	if strings.Contains(got, "gno.land/p/b") {
		t.Errorf("reported b, which has no successor:\n%s", got)
	}
	// It must say that moving is editing an import, because the absence of a
	// `gnopm update` is the thing a newcomer will otherwise go looking for.
	if !strings.Contains(errb.String(), "editing the import") {
		t.Errorf("no advice about what to do next:\n%s", errb.String())
	}
}

// TestOutdatedLooksPastAHole. bump moves one number at a time, but a stack of
// pull requests can skip one: the README documents v0 to v2 with v1 existing
// nowhere. Probing only the next number would stop at that hole and report
// nothing, which is wrong in the direction that matters.
func TestOutdatedLooksPastAHole(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	f := newFakeChain(t)
	f.live["gno.land/p/a/v2"] = true // v1 was skipped and exists nowhere

	var out, errb bytes.Buffer
	if err := Run([]string{"outdated", "-C", root, "-rpc", f.srv.URL, "-chainid", "test"}, &out, &errb); err != nil {
		t.Fatalf("outdated: %v\n%s", err, errb.String())
	}
	if !strings.Contains(out.String(), "gno.land/p/a/v2") {
		t.Errorf("stopped at the hole:\n%s", out.String())
	}
}

// TestOutdatedIsQuietWhenCurrent: an empty answer has to pipe as empty, so the
// "nothing to do" goes to stderr and stdout stays clean.
func TestOutdatedIsQuietWhenCurrent(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	f := newFakeChain(t)

	var out, errb bytes.Buffer
	if err := Run([]string{"outdated", "-C", root, "-rpc", f.srv.URL, "-chainid", "test"}, &out, &errb); err != nil {
		t.Fatalf("outdated: %v", err)
	}
	if out.String() != "" {
		t.Errorf("an empty answer wrote to stdout: %q", out.String())
	}
	if !strings.Contains(errb.String(), "newest version") {
		t.Errorf("said nothing on stderr:\n%s", errb.String())
	}

	// -json still has to produce a parseable empty answer rather than null.
	var jout, jerr bytes.Buffer
	if err := Run([]string{"outdated", "-C", root, "-json", "-rpc", f.srv.URL, "-chainid", "test"}, &jout, &jerr); err != nil {
		t.Fatalf("outdated -json: %v", err)
	}
	if strings.TrimSpace(jout.String()) != "[]" {
		t.Errorf("-json on an empty answer printed %q", jout.String())
	}
}

// TestOutdatedIgnoresAnUnversionedModule. An unversioned module line is legal,
// and r/moul/home relies on it, so it has no successor to look for and must not
// make the command fail or invent one.
func TestOutdatedIgnoresAnUnversionedModule(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "r/home", "gno.land/r/home", "package home\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	f := newFakeChain(t)

	var out, errb bytes.Buffer
	if err := Run([]string{"outdated", "-C", root, "-rpc", f.srv.URL, "-chainid", "test"}, &out, &errb); err != nil {
		t.Fatalf("outdated: %v\n%s", err, errb.String())
	}
	if !strings.Contains(errb.String(), "carries a version") {
		t.Errorf("did not explain that there was nothing to check:\n%s", errb.String())
	}
}
