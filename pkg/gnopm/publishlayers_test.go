package gnopm

import (
	"strings"
	"testing"
)

func mkPlan(mod string) plan {
	return plan{pkg: Package{Module: mod}, state: StateAbsent}
}

func layerNames(layers [][]plan) []string {
	var out []string
	for _, l := range layers {
		var names []string
		for _, pl := range l {
			names = append(names, pl.pkg.Module)
		}
		out = append(out, strings.Join(names, ","))
	}
	return out
}

// The whole point: independent packages share a layer, and therefore a
// signature. A dependent is never in the same layer as what it imports.
func TestLayerPlansGroupsIndependentPackages(t *testing.T) {
	plans := []plan{mkPlan("a"), mkPlan("b"), mkPlan("ab"), mkPlan("abc")}
	deps := map[string][]string{
		"a":   nil,
		"b":   nil,
		"ab":  {"a", "b"},
		"abc": {"ab"},
	}
	layers, err := layerPlans(plans, deps)
	if err != nil {
		t.Fatal(err)
	}
	got := layerNames(layers)
	want := []string{"a,b", "ab", "abc"}
	if len(got) != len(want) {
		t.Fatalf("layers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("layer %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A flat set of unrelated packages is one layer, which is the case that turns
// 80 passphrase prompts into one.
func TestLayerPlansFlattensWhenNothingDependsOnAnything(t *testing.T) {
	var plans []plan
	for _, m := range []string{"a", "b", "c", "d", "e"} {
		plans = append(plans, mkPlan(m))
	}
	layers, err := layerPlans(plans, map[string][]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 || len(layers[0]) != 5 {
		t.Fatalf("layers = %v, want one layer of five", layerNames(layers))
	}
}

// Imports outside the publish set are already live or somebody else's, so they
// must not push a package into a later layer.
func TestLayerPlansIgnoresDepsOutsideTheSet(t *testing.T) {
	plans := []plan{mkPlan("a")}
	deps := map[string][]string{"a": {"gno.land/p/nt/avl/v0", "gno.land/p/someone/else"}}
	layers, err := layerPlans(plans, deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 || layers[0][0].pkg.Module != "a" {
		t.Fatalf("layers = %v, want a single layer holding a", layerNames(layers))
	}
}

// Only what this run will actually send is layered: a live package is not a
// dependency that orders anything, and a blocked one is not going up at all.
func TestLayerPlansSkipsWhatIsNotBeingPublished(t *testing.T) {
	live := mkPlan("live")
	live.state = StateLive
	blocked := mkPlan("blocked")
	blocked.missing = []depBlock{{module: "x", why: "absent"}}
	parked := mkPlan("parked")
	parked.state = StateParked

	layers, err := layerPlans([]plan{live, blocked, parked, mkPlan("a")}, map[string][]string{
		"a": {"live"}, // already on chain, so it does not order this run
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 || len(layers[0]) != 1 || layers[0][0].pkg.Module != "a" {
		t.Fatalf("layers = %v, want a single layer holding only a", layerNames(layers))
	}
}

func TestLayerPlansNothingToDo(t *testing.T) {
	layers, err := layerPlans(nil, nil)
	if err != nil || layers != nil {
		t.Fatalf("layerPlans(nil) = %v, %v; want nil, nil", layers, err)
	}
}

// A cycle is reported, never silently dropped: publishing a subset of a cycle
// would leave the chain in a state neither the tree nor the run describes.
func TestLayerPlansRejectsACycle(t *testing.T) {
	plans := []plan{mkPlan("a"), mkPlan("b")}
	deps := map[string][]string{"a": {"b"}, "b": {"a"}}
	_, err := layerPlans(plans, deps)
	if err == nil {
		t.Fatal("a cycle must be an error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error %q should name the cycle", err)
	}
}

// A self-import is not a cycle and must not stall the layering.
func TestLayerPlansToleratesSelfImport(t *testing.T) {
	layers, err := layerPlans([]plan{mkPlan("a")}, map[string][]string{"a": {"a"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 {
		t.Fatalf("layers = %v, want one", layerNames(layers))
	}
}

func TestAddrOfParsesGnokeyList(t *testing.T) {
	listing := `0. dev (local) - addr: g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5 pub: gpub1..., path: <nil>
1. moul (local) - addr: g1manfred47kzduec920z88wfr64ylksmdcedlf5 pub: gpub1..., path: 44'/118'/0'/0/0`

	if got := addrOf(listing, "moul"); got != "g1manfred47kzduec920z88wfr64ylksmdcedlf5" {
		t.Errorf("addrOf(moul) = %q", got)
	}
	if got := addrOf(listing, "dev"); got != "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5" {
		t.Errorf("addrOf(dev) = %q", got)
	}
	// A prefix of a real name is not that name.
	if got := addrOf(listing, "mou"); got != "" {
		t.Errorf("addrOf(mou) = %q, want empty", got)
	}
	if got := addrOf("", "moul"); got != "" {
		t.Errorf("addrOf on empty listing = %q", got)
	}
}

func TestCreatorForPrefersExplicitAddress(t *testing.T) {
	const addr = "g1manfred47kzduec920z88wfr64ylksmdcedlf5"
	got, err := creatorFor("gnokey", "moul", addr)
	if err != nil || got != addr {
		t.Fatalf("creatorFor(-addr) = %q, %v", got, err)
	}
	// A key that is already an address answers itself, without running gnokey.
	got, err = creatorFor("/nonexistent-gnokey", addr, "")
	if err != nil || got != addr {
		t.Fatalf("creatorFor(address as key) = %q, %v", got, err)
	}
	if _, err := creatorFor("gnokey", "moul", "not-an-address"); err == nil {
		t.Fatal("a non-address -addr must be rejected")
	}
}

// wideRepo is four packages that do not import each other, plus one that
// imports two of them: two layers, five packages. It is the shape a workspace
// actually has, and the one layering exists for.
func wideRepo(t *testing.T) string {
	t.Helper()
	root := newRepo(t)
	for _, n := range []string{"alpha", "beta", "gamma", "delta"} {
		addPkg(t, root, "p/moul/"+n, "gno.land/p/moul/"+n+"/v0",
			"package "+n+"\n\nfunc F() string { return \""+n+"\" }\n")
	}
	addPkg(t, root, "r/moul/app", "gno.land/r/moul/app/v0",
		"package app\n\nimport (\n\t\"gno.land/p/moul/alpha/v0\"\n\t\"gno.land/p/moul/beta/v0\"\n)\n\nfunc A() string { return alpha.F() + beta.F() }\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	return root
}

// The headline: five packages, two signatures instead of five.
func TestPublishBatchesByLayerByDefault(t *testing.T) {
	root := wideRepo(t)
	f := newFakeChain(t)

	ran, report, err := publishRun(t, root, f, 0, "-addr", testCreator)
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, report)
	}
	if len(ran) != 4 {
		t.Fatalf("ran %d command(s), want two sign/broadcast pairs:\n%+v", len(ran), ran)
	}
	for i := 0; i < len(ran); i += 2 {
		if ran[i].args[0] != "sign" || ran[i+1].args[0] != "broadcast" {
			t.Fatalf("pair %d is %q then %q", i/2, ran[i].args[0], ran[i+1].args[0])
		}
	}
	if !strings.Contains(report, "2 dependency layer(s)") {
		t.Errorf("the report does not say how many layers:\n%s", report)
	}
}

// The opt-out moul asked for: a transaction each, the old shape.
func TestPublishOneTxPerPackageOptsOut(t *testing.T) {
	root := wideRepo(t)
	f := newFakeChain(t)

	ran, report, err := publishRun(t, root, f, 0, "-addr", testCreator, "-one-tx-per-package")
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, report)
	}
	if len(ran) != 5 {
		t.Fatalf("ran %d command(s), want one per package:\n%+v", len(ran), ran)
	}
	for _, c := range ran {
		if c.args[0] != "maketx" || c.args[1] != "addpkg" {
			t.Fatalf("want maketx addpkg, got %v", c.args)
		}
	}
}

// Not knowing which address signs must cost prompts, never the publish.
func TestPublishFallsBackWhenTheAddressIsUnknown(t *testing.T) {
	root := wideRepo(t)
	f := newFakeChain(t)

	// No -addr, and -key is a name the (absent) gnokey cannot resolve.
	ran, report, err := publishRun(t, root, f, 0, "-key", "nosuchkey", "-gnokey-cmd", "/nonexistent-gnokey")
	if err != nil {
		t.Fatalf("publish must not refuse over a name lookup: %v\n%s", err, report)
	}
	if len(ran) != 5 {
		t.Fatalf("ran %d command(s), want one per package after the fallback:\n%+v", len(ran), ran)
	}
	if !strings.Contains(report, "one transaction per package") {
		t.Errorf("the fallback is silent, and it changes what you pay:\n%s", report)
	}
	if !strings.Contains(report, "-addr") {
		t.Errorf("the report does not say how to get batching back:\n%s", report)
	}
}

// A line per package is 200 lines in a real workspace, and almost all of them
// say "live", which is the one state needing no decision.
func TestPublishReportIsQuietAboutLivePackages(t *testing.T) {
	root := wideRepo(t)
	f := newFakeChain(t)
	f.live["gno.land/p/moul/alpha/v0"] = true
	f.live["gno.land/p/moul/beta/v0"] = true

	_, report, err := publishRun(t, root, f, 0, "-addr", testCreator, "-print")
	if err != nil {
		t.Fatalf("publish -print: %v\n%s", err, report)
	}
	if strings.Contains(report, "gno.land/p/moul/alpha/v0") {
		t.Errorf("a live package is listed by default:\n%s", report)
	}
	if !strings.Contains(report, "2 package(s) already on chain") {
		t.Errorf("the report does not account for the live ones:\n%s", report)
	}
	// What is going up is still named, because that is the decision.
	if !strings.Contains(report, "gno.land/p/moul/gamma/v0") {
		t.Errorf("an absent package is not listed:\n%s", report)
	}

	_, verbose, err := publishRun(t, root, f, 0, "-addr", testCreator, "-print", "-v")
	if err != nil {
		t.Fatalf("publish -v: %v\n%s", err, verbose)
	}
	if !strings.Contains(verbose, "gno.land/p/moul/alpha/v0") {
		t.Errorf("-v does not list the live packages:\n%s", verbose)
	}
}
