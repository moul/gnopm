package gnopm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// blockedRepo is the shape that made -skip-blocked necessary, reduced to five
// packages: one import nothing can supply, a chain of two dependents behind
// it, and two packages that have nothing to do with any of it.
//
//	ghost -> gno.land/p/stranger/thing/v0   (not in the tree, not on the chain)
//	mid   -> ghost
//	app   -> mid
//	alpha, beta                              (independent, publishable)
//
// Two hops on purpose: one hop could be special-cased, two cannot, and the
// real workspace will grow a second hop the day somebody imports one of the
// five.
func blockedRepo(t *testing.T) string {
	t.Helper()
	root := newRepo(t)
	addPkg(t, root, "p/moul/ghost", "gno.land/p/moul/ghost/v0",
		"package ghost\n\nimport \"gno.land/p/stranger/thing/v0\"\n\nfunc G() string { return thing.T() }\n")
	addPkg(t, root, "p/moul/mid", "gno.land/p/moul/mid/v0",
		"package mid\n\nimport \"gno.land/p/moul/ghost/v0\"\n\nfunc M() string { return ghost.G() }\n")
	addPkg(t, root, "r/moul/app", "gno.land/r/moul/app/v0",
		"package app\n\nimport \"gno.land/p/moul/mid/v0\"\n\nfunc A() string { return mid.M() }\n")
	addPkg(t, root, "p/moul/alpha", "gno.land/p/moul/alpha/v0",
		"package alpha\n\nfunc F() string { return \"alpha\" }\n")
	addPkg(t, root, "p/moul/beta", "gno.land/p/moul/beta/v0",
		"package beta\n\nfunc F() string { return \"beta\" }\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	return root
}

// TestPublishSkipBlockedPublishesTheRest.
//
// What went wrong: `gnopm publish` on moul/gno-contracts found 311 packages to
// publish and 5 it could not, and emitted NOTHING: no plan, no documents, no
// commands. The five import a v0 that lives only in mainnet history, so no
// fresh chain can have it and nothing in the workspace can produce it. The
// diagnosis was right and the blast radius was not: a plan costs nothing and
// signs nothing, and one refusal stopped 306 publishable packages from being
// planned at all.
//
// Why no existing test could see it: every blocked-dependency test asserted
// only that the run refused, which it still does by default. Nothing asserted
// what a blocked run does to the packages that are fine.
func TestPublishSkipBlockedPublishesTheRest(t *testing.T) {
	root := blockedRepo(t)
	f := newFakeChain(t)

	txPath := filepath.Join(t.TempDir(), "tx.json")
	_, report, err := publishPlan(t, root, f, "-skip-blocked", "-addr", testCreator, "-o", txPath)
	if err == nil {
		t.Fatalf("a run that left work out must not exit 0:\n%s", report)
	}
	if got := ExitCode(err); got != ExitPartial {
		t.Fatalf("exit code = %d, want %d: published-what-it-could is neither success nor failure", got, ExitPartial)
	}

	// Asserted against the document, not the report: the document is what
	// gets signed, and a report that says the right thing over a transaction
	// that carries the wrong messages is the failure worth catching.
	doc, err := os.ReadFile(txPath)
	if err != nil {
		t.Fatalf("no transaction document was written at all: %v\n%s", err, report)
	}
	for _, m := range []string{"gno.land/p/moul/alpha/v0", "gno.land/p/moul/beta/v0"} {
		if !strings.Contains(string(doc), m) {
			t.Errorf("%s was dropped along with the blocked ones:\n%s", m, report)
		}
	}
	// And nothing behind the blockage is in it, transitively, two hops deep.
	for _, m := range []string{"gno.land/p/moul/ghost/v0", "gno.land/p/moul/mid/v0", "gno.land/r/moul/app/v0"} {
		if strings.Contains(string(doc), m) {
			t.Errorf("%s is in the signed document and cannot possibly work:\n%s", m, report)
		}
	}
	if !strings.Contains(report, "1 transaction(s) for 2 package(s)") {
		t.Errorf("the plan does not say it is publishing the two that are fine:\n%s", report)
	}
}

// Quietly dropping work is worse than refusing it, so every skipped package is
// named, with the dependency that stops it, and the transitive ones name their
// IMMEDIATE blocker rather than the root cause three hops away.
func TestPublishSkipBlockedNamesEverythingItLeftOut(t *testing.T) {
	root := blockedRepo(t)
	f := newFakeChain(t)

	_, report, err := publishPlan(t, root, f, "-skip-blocked", "-addr", testCreator)
	if ExitCode(err) != ExitPartial {
		t.Fatalf("publish: %v\n%s", err, report)
	}
	for _, want := range []string{
		"3 package(s) NOT published",
		"p/moul/ghost/v0",
		"needs p/stranger/thing/v0: not live, and not in this workspace",
		"needs p/moul/ghost/v0: skipped: it is blocked itself",
		"needs p/moul/mid/v0: skipped: it is blocked itself",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report never says %q:\n%s", want, report)
		}
	}
}

// The default is unchanged, and the refusal now says how to get past it.
func TestPublishWithoutSkipBlockedStillRefuses(t *testing.T) {
	root := blockedRepo(t)
	f := newFakeChain(t)

	script, report, err := publishPlan(t, root, f, "-addr", testCreator)
	if err == nil {
		t.Fatalf("publish emitted a plan for a workspace with a blocked package:\n%s", report)
	}
	if got := ExitCode(err); got != ExitFailure {
		t.Fatalf("exit code = %d, want %d: a refusal did nothing and must say so", got, ExitFailure)
	}
	if strings.TrimSpace(script) != "" {
		t.Errorf("a refusal wrote a script:\n%s", script)
	}
	// The refusal is the returned error, which is what the process prints and
	// exits on; the report above it is the listing.
	if !strings.Contains(err.Error(), "-skip-blocked") {
		t.Errorf("the refusal does not name the way past it: %v", err)
	}
	// Only the package with the unsatisfiable import is blocked by default:
	// the transitive walk is what -skip-blocked turns on, and turning it on
	// unconditionally would change what the default run reports.
	if !strings.Contains(err.Error(), "1 package(s) blocked") {
		t.Errorf("the default run counts more than the one genuinely blocked package: %v", err)
	}
	if strings.Contains(report, "SKIPPED") {
		t.Errorf("the default run reported packages as skipped:\n%s", report)
	}
}

// -skip-blocked on a workspace with nothing blocked is exit 0. The code says
// "left work out", not "the flag was passed", or every CI job that sets it
// defensively would go red forever.
func TestPublishSkipBlockedExitsZeroWhenNothingIsBlocked(t *testing.T) {
	root := wideRepo(t)
	f := newFakeChain(t)

	_, report, err := publishPlan(t, root, f, "-skip-blocked", "-addr", testCreator)
	if err != nil {
		t.Fatalf("publish -skip-blocked with nothing blocked: %v (exit %d)\n%s", err, ExitCode(err), report)
	}
	if strings.Contains(report, "NOT published") {
		t.Errorf("a clean run printed a skipped block:\n%s", report)
	}
}

// markSkipped works off the graph, so a diamond skips both sides of it and
// leaves the unrelated branch alone. Unit-level because the integration tests
// above can only afford one shape, and the shape they cannot reach is the one
// where a package has a good dependency and a bad one.
func TestMarkSkippedPropagatesThroughTheGraph(t *testing.T) {
	bad := mkPlan("bad")
	bad.missing = []depBlock{{module: "outside", why: "not live, and not in this workspace"}}
	live := mkPlan("live")
	live.state = StateLive
	// Topological order: dependencies before dependents.
	plans := []plan{bad, live, mkPlan("good"), mkPlan("mixed"), mkPlan("far"), mkPlan("clean")}
	deps := map[string][]string{
		"bad":   {"outside"},
		"good":  nil,
		"mixed": {"good", "bad"}, // one fine dependency and one that is leaving
		"far":   {"mixed"},       // two hops from the blockage
		"clean": {"good", "live"},
	}
	markSkipped(plans, deps)

	skipped := map[string]bool{}
	for _, pl := range plans {
		if len(pl.missing) > 0 {
			skipped[pl.pkg.Module] = true
		}
	}
	for _, m := range []string{"bad", "mixed", "far"} {
		if !skipped[m] {
			t.Errorf("%s should be skipped", m)
		}
	}
	for _, m := range []string{"good", "clean", "live"} {
		if skipped[m] {
			t.Errorf("%s has nothing to do with the blockage and must stay in the plan", m)
		}
	}
	// The reason names the immediate dependency, not the root cause.
	for _, pl := range plans {
		if pl.pkg.Module == "far" && pl.missing[0].module != "mixed" {
			t.Errorf("far is blamed on %q, want its own import mixed", pl.missing[0].module)
		}
	}
}

// A live package whose tree copy has since grown an unsatisfiable import is
// blocked, but it is not MISSING: the chain has it, so nothing importing it
// has to wait for this run and nothing importing it may be skipped for it.
func TestMarkSkippedDoesNotPropagateFromALivePackage(t *testing.T) {
	live := mkPlan("live")
	live.state = StateLive
	live.missing = []depBlock{{module: "outside", why: "not live, and not in this workspace"}}
	plans := []plan{live, mkPlan("dependent")}
	markSkipped(plans, map[string][]string{"dependent": {"live"}})

	if len(plans[1].missing) != 0 {
		t.Errorf("dependent was skipped for a dependency that is already on chain: %v", plans[1].missing)
	}
}
