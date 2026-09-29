package gnopm

import (
	"strings"
	"testing"
)

// TestPublishReportTrimsTheSharedDomain: every row of a publish shares the
// domain element, so it is printed once above the list and nowhere else.
//
// Worth pinning because the saving is invisible in a two-package fixture and
// the reason for the change was a 200-row workspace, where it is nine
// characters times every row. A regression would not look like a bug, it would
// look like slightly longer lines.
func TestPublishReportTrimsTheSharedDomain(t *testing.T) {
	root := txWorkspace(t)
	f := newFakeChain(t)

	_, report, err := publishPlan(t, root, f, "-addr", testCreator)
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, report)
	}
	if !strings.Contains(report, "\ngno.land/\n") {
		t.Errorf("the shared domain is not printed once, above the list:\n%s", report)
	}
	if strings.Contains(report, "+ gno.land/p/moul/one/v0") {
		t.Errorf("a row still repeats the domain:\n%s", report)
	}
	if !strings.Contains(report, "p/moul/one/v0") {
		t.Errorf("the package is not listed at all:\n%s", report)
	}
}

// TestPublishReportNamesTheTransactionPerPackage: the row says which
// transaction it lands in, which is what replaced a per-transaction membership
// block that listed every path a second time.
//
// The one-transaction case deliberately annotates nothing: a "tx1" on every
// row of a single-transaction plan is a column that cannot distinguish
// anything, which is the kind of line people stop reading.
func TestPublishReportNamesTheTransactionPerPackage(t *testing.T) {
	root := txWorkspace(t) // two/v0 imports one/v0, so two layers
	f := newFakeChain(t)

	_, report, err := publishPlan(t, root, f, "-addr", testCreator)
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, report)
	}
	for _, want := range []string{"p/moul/one/v0", "tx1", "r/moul/two/v0", "tx2"} {
		if !strings.Contains(report, want) {
			t.Fatalf("the report does not name %q:\n%s", want, report)
		}
	}
	if strings.Index(report, "tx1") > strings.Index(report, "tx2") {
		t.Errorf("the dependency is not in the earlier transaction:\n%s", report)
	}

	// One layer, one transaction, no column.
	single := newRepo(t)
	addPkg(t, single, "p/solo", "gno.land/p/moul/solo/v0", "package solo\n")
	commit(t, single, "initial")
	mustRun(t, single, "sync")
	_, one, err := publishPlan(t, single, newFakeChain(t), "-addr", testCreator)
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, one)
	}
	if strings.Contains(one, "tx1") {
		t.Errorf("a single-transaction plan annotates a column that says nothing:\n%s", one)
	}
}

// TestReportIsPlainWhenNobodyIsWatching: the report is read by people and
// parsed by tests, and an escape sequence in a buffer would break the second
// without helping the first.
func TestReportIsPlainWhenNobodyIsWatching(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "")
	root := txWorkspace(t)
	f := newFakeChain(t)

	_, report, err := publishPlan(t, root, f, "-addr", testCreator)
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, report)
	}
	if strings.Contains(report, "\x1b[") {
		t.Fatalf("SGR escapes reached a buffer:\n%q", report)
	}
}

// TestColorDecision pins the three environment rules in the order they win.
// Each has bitten somebody: CI logs that render colour set CLICOLOR_FORCE,
// NO_COLOR is the one users expect to be absolute, and TERM=dumb is the
// terminal saying so itself. Split from isTerminal so that the rule can be
// tested without a pseudo-terminal, which is the only reason TERM=dumb has a
// case here at all.
func TestColorDecision(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		tty  bool
		want bool
	}{
		{name: "a pipe", want: false},
		{name: "a terminal", tty: true, want: true},
		{name: "forced into a pipe", env: map[string]string{"CLICOLOR_FORCE": "1"}, want: true},
		{name: "forced but zero", env: map[string]string{"CLICOLOR_FORCE": "0"}, want: false},
		{name: "NO_COLOR on a terminal", env: map[string]string{"NO_COLOR": "1"}, tty: true, want: false},
		{name: "NO_COLOR empty is not set", env: map[string]string{"NO_COLOR": ""}, tty: true, want: true},
		{name: "force beats NO_COLOR", env: map[string]string{"CLICOLOR_FORCE": "1", "NO_COLOR": "1"}, want: true},
		{name: "dumb terminal", env: map[string]string{"TERM": "dumb"}, tty: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLICOLOR_FORCE", "")
			t.Setenv("NO_COLOR", "")
			t.Setenv("TERM", "xterm")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := colorDecision(tc.tty); got != tc.want {
				t.Errorf("colorDecision(%v) = %v, want %v", tc.tty, got, tc.want)
			}
		})
	}
}

// paint is the only place colour is added, so a style with an empty code, or
// an Env that is not colouring, has to be the identity function: every report
// line is one expression either way and a stray escape would land in all of
// them.
func TestPaintIsTheIdentityWithoutColor(t *testing.T) {
	plain := &Env{}
	if got := plain.bold("x"); got != "x" {
		t.Errorf("bold on a plain Env = %q", got)
	}
	colored := &Env{Color: true}
	if got := colored.bold("x"); got != "\x1b[1mx\x1b[0m" {
		t.Errorf("bold = %q", got)
	}
	if got := colored.paint("", "x"); got != "x" {
		t.Errorf("an empty style painted: %q", got)
	}
	if got := colored.dim(""); got != "" {
		t.Errorf("an empty string was wrapped: %q", got)
	}
}
