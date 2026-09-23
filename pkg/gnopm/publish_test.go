package gnopm

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Payload must count exactly what MsgAddPackage carries. Miscounting it
// mis-sizes gas and the storage deposit, and both are paid in real money.
func TestPayloadCountsWhatAddpkgUploads(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "home.gno"), "package home\n")      // 13
	write(t, filepath.Join(dir, "home_test.gno"), "package home\n") // 13, tests DO travel
	write(t, filepath.Join(dir, "README.md"), "hi\n")               // 3, so does the README
	write(t, filepath.Join(dir, "gnomod.toml"), "module = \"x\"\n") // 13
	write(t, filepath.Join(dir, "LICENSE"), "MIT\n")                // 4, whole-name match
	write(t, filepath.Join(dir, "Makefile"), "all:\n")              // excluded: extension
	write(t, filepath.Join(dir, ".hidden.md"), "x\n")               // excluded: dot-file
	write(t, filepath.Join(dir, "content", "bio.md"), "never counted\n")

	files, total, err := Payload(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if len(files) != 5 {
		t.Fatalf("payload = %v, want the 5 uploadable files", names)
	}
	// bodies 13+13+3+13+4 = 46; names 8+13+9+11+7 = 48
	if want := 94; total != want {
		t.Errorf("total = %d, want %d (bodies plus names)", total, want)
	}
	if files[0].Name != "gnomod.toml" && files[0].Size != 13 {
		t.Errorf("not sorted biggest first: %v", names)
	}
}

// Only non-test imports gate a deploy: a test-only import travels with the
// package but the VM never runs it, so requiring it on chain would block a
// perfectly valid upload.
func TestImportsSkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.gno"),
		"package a\nimport (\n\t\"gno.land/p/x/one/v0\"\n\t\"gno.land/p/x/two/v1\"\n)\n")
	write(t, filepath.Join(dir, "a_test.gno"), "package a\nimport \"gno.land/p/x/testonly/v0\"\n")
	write(t, filepath.Join(dir, "sub", "deep.gno"), "package sub\nimport \"gno.land/p/x/deep/v0\"\n")

	got, err := Imports(dir, "gno.land")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gno.land/p/x/one/v0", "gno.land/p/x/two/v1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Imports = %v, want %v (no test import, no sub-directory)", got, want)
	}
	// A different domain is somebody else's chain, not ours to order.
	if got, _ := Imports(dir, "example.com"); len(got) != 0 {
		t.Errorf("Imports(example.com) = %v, want none", got)
	}
}

// A dependency must be published before whatever imports it, or the second
// transaction fails on a path the chain has never heard of.
func TestTopoOrderPutsDependenciesFirst(t *testing.T) {
	pkgs := []Package{
		{Module: "app", Dir: "app"},
		{Module: "lib", Dir: "lib"},
		{Module: "base", Dir: "base"},
		{Module: "loner", Dir: "loner"},
	}
	deps := map[string][]string{
		"app":  {"lib", "outside/the/set"},
		"lib":  {"base"},
		"base": nil,
	}
	got := TopoOrder(pkgs, deps)
	if len(got) != len(pkgs) {
		t.Fatalf("TopoOrder dropped packages: %v", got)
	}
	pos := map[string]int{}
	for i, p := range got {
		pos[p.Module] = i
	}
	if pos["base"] > pos["lib"] || pos["lib"] > pos["app"] {
		var order []string
		for _, p := range got {
			order = append(order, p.Module)
		}
		t.Errorf("order = %v, want base before lib before app", order)
	}
}

// Defensive: a cycle cannot arise from gno imports, but it must not hang or
// silently drop a package if one ever does.
func TestTopoOrderSurvivesACycle(t *testing.T) {
	pkgs := []Package{{Module: "a"}, {Module: "b"}}
	deps := map[string][]string{"a": {"b"}, "b": {"a"}}
	if got := TopoOrder(pkgs, deps); len(got) != 2 {
		t.Fatalf("TopoOrder = %v, want both packages", got)
	}
}

// The fee is a ratio against gas_wanted, not an absolute: the mempool compares
// fee/gas_wanted, and gas_fee is deducted in full and never refunded.
func TestFeeForIsARatioAndNeverZero(t *testing.T) {
	for gas, want := range map[int64]string{
		10_000_000: "30000ugnot",
		53_182_800: "159548ugnot",
		1:          "1ugnot",
		0:          "1ugnot",
	} {
		if got := FeeFor(gas); got != want {
			t.Errorf("FeeFor(%d) = %s, want %s", gas, got, want)
		}
	}
	const gas = 20_000_000
	fee, err := strconv.ParseInt(strings.TrimSuffix(FeeFor(gas), "ugnot"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	// gnoland-1 reports 0.001 ugnot/gas (auth/gasprice, h271568). Three times
	// that is margin against the dynamic block gas price without taxing every
	// package for a congestion this workload cannot cause.
	if r := float64(fee) / float64(gas); r < 0.0029 || r > 0.0031 {
		t.Errorf("ratio %.4f ugnot/gas, want ~0.003, three times the chain floor", r)
	}
}

// gasSamples are real mainnet add_package transactions, read back from the
// tx-indexer on 2026-09-23 with the exact bytes they uploaded. They are the
// cases that matter: the ones a per-byte ceiling gets wrong.
//
// The point of keeping them here is that GasFor is an envelope over measured
// reality, and an envelope with no measurements next to it decays into a
// number nobody dares change.
var gasSamples = []struct {
	path  string
	bytes int
	used  int64
	// heavyInit marks a system realm whose init() builds a large tree at
	// deploy time. These are deliberately OUTSIDE the envelope: four of the
	// 464 samples are like this, all of them r/gov/dao/*, r/sys/* or
	// r/gnoland/wugnot, and sizing for them would tax every ordinary package
	// to insure against one no workspace can publish.
	heavyInit bool
}{
	// The one that actually failed a publish: 4 KB, and 1800/byte funded 80%
	// of what it needed.
	{"gno.land/p/moul/agents/commit/v0", 4007, 9_049_792, false},
	// Small packages are where a per-byte ceiling is worst, because the fixed
	// cost is the whole bill: this one needs 12,821 gas per uploaded byte.
	{"gno.land/r/g1r6luttvrkksxh9h4asjq2qd8zsd5nlkrzvyjur/pgstate", 208, 2_666_727, false},
	{"gno.land/r/sys/rewards", 270, 3_688_651, false},
	{"gno.land/r/tests/vm/subtests", 576, 3_302_965, false},
	{"gno.land/r/moul/demo/hello/v0", 754, 5_930_943, false},
	// Large ones, where the old figure worked and the constant term is
	// amortized away. The ceiling must still cover them.
	{"gno.land/r/samcrew/home", 40_763, 67_591_249, false},
	{"gno.land/r/sys/namereg/v0", 50_489, 153_543_375, true},
}

// TestGasForCoversWhatTheChainActuallyCharged is the regression the old
// per-byte ceiling could not pass: gas is a fixed cost plus a per-byte cost,
// and sizing it from bytes alone under-funds every small package.
func TestGasForCoversWhatTheChainActuallyCharged(t *testing.T) {
	for _, s := range gasSamples {
		got := GasFor(s.bytes)
		if s.heavyInit {
			// Documented as out of envelope. Asserted the other way round so
			// that widening the envelope to cover it is a deliberate edit here
			// and not an accident nobody notices.
			if got >= s.used {
				t.Errorf("GasFor(%d) = %d now covers heavy-init %s (%d gas); "+
					"if that was intended, drop its heavyInit flag",
					s.bytes, got, s.path, s.used)
			}
			continue
		}
		if got < s.used {
			t.Errorf("GasFor(%d) = %d, below the %d gas the chain charged for %s",
				s.bytes, got, s.used, s.path)
		}
		if got > 12*s.used {
			t.Errorf("GasFor(%d) = %d, more than 12x the %d actually used for %s: "+
				"gas_wanted is not charged, but the fee ratio is taken against it",
				s.bytes, got, s.used, s.path)
		}
	}
}

// A per-byte-only ceiling is not merely tight, it is the wrong shape. This
// pins the reason the constant changed, so reverting it fails loudly.
func TestGasForIsNotProportionalToBytes(t *testing.T) {
	small, big := GasFor(200), GasFor(400)
	if big >= 2*small {
		t.Errorf("GasFor(400)=%d vs GasFor(200)=%d: doubling the payload must not "+
			"double the ceiling, the fixed cost dominates a small package", big, small)
	}
	if GasFor(0) < 1_000_000 {
		t.Errorf("GasFor(0) = %d, but an empty package still pays to be "+
			"parsed, type-checked and initialised", GasFor(0))
	}
}

// gas_wanted above the block limit is rejected outright, so the ceiling clamps.
func TestGasForClampsToABlock(t *testing.T) {
	if got := GasFor(1 << 30); got != maxBlockGas {
		t.Errorf("GasFor(huge) = %d, want the block limit %d", got, maxBlockGas)
	}
	if got := GasFor(-1); got != gasFixed {
		t.Errorf("GasFor(-1) = %d, want the fixed cost %d", got, gasFixed)
	}
}

// Omitting max_deposit is not opting out: it falls back to the chain default,
// 100 GNOT of ceiling per message on gno.land. The ceiling is refundable, so
// headroom is free, but it must cover the source lock.
func TestDepositForCoversTheSourceLock(t *testing.T) {
	const bytes = 29_546
	got := DepositFor(bytes)
	if lock := int64(bytes) * storagePerByte; got <= lock {
		t.Errorf("DepositFor(%d) = %d, does not cover the %d lock", bytes, got, lock)
	}
	if got%1_000_000 != 0 {
		t.Errorf("DepositFor = %d, want whole GNOT", got)
	}
	if min := DepositFor(1); min < 5_000_000 {
		t.Errorf("floor = %d, want at least 5 GNOT", min)
	}
}

// Architecture decision 4: the path says which chain, so no endpoint table.
func TestHostForPath(t *testing.T) {
	for in, want := range map[string]string{
		"gno.land/r/moul/home":    "https://gno.land",
		"gno.land/p/nt/avl/v0":    "https://gno.land",
		"example.com/r/x/y":       "https://example.com",
		"test5.gno.land/r/demo/a": "https://test5.gno.land",
	} {
		got, err := hostForPath(in)
		if err != nil {
			t.Errorf("hostForPath(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("hostForPath(%q) = %q, want %q", in, got, want)
		}
	}
	// A path with no domain cannot name a chain, and guessing one would send
	// a transaction somewhere nobody asked for.
	for _, bad := range []string{"", "nodomain/r/x", "justaword"} {
		if _, err := hostForPath(bad); err == nil {
			t.Errorf("hostForPath(%q) should fail", bad)
		}
	}
}

// Discovery is skipped entirely when both are given, so a local gnodev with
// no gnoweb in front of it still works, offline.
func TestDiscoverChainHonoursOverridesWithoutNetwork(t *testing.T) {
	c, err := DiscoverChain(&Env{}, "nodomain/whatever", "http://127.0.0.1:26657", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if c.RPC != "http://127.0.0.1:26657" || c.ID != "dev" {
		t.Errorf("DiscoverChain = %+v", c)
	}
}

// The meta tags are the contract with gnoweb; a silent change to either name
// would turn discovery into a confusing network error.
func TestChainMetaTagsParse(t *testing.T) {
	page := `<head>
  <meta name="gnoconnect:rpc" content="https://rpc.gno.land" />
  <meta name="gnoconnect:chainid" content="gnoland-1" />
</head>`
	m := rpcMeta.FindStringSubmatch(page)
	if m == nil || m[1] != "https://rpc.gno.land" {
		t.Errorf("rpc meta = %v", m)
	}
	m = chainIDMeta.FindStringSubmatch(page)
	if m == nil || m[1] != "gnoland-1" {
		t.Errorf("chainid meta = %v", m)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"plain":             "'plain'",
		"with space":        "'with space'",
		"it's":              `'it'\''s'`,
		"a;rm -rf /":        "'a;rm -rf /'",
		"gno.land/r/x/y/v0": "'gno.land/r/x/y/v0'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// Detect, do not ask: a required -key would be friction paid on every
// invocation to restate what the package path already says. A gno namespace
// is its owner, so the key name is the third path element.
func TestNamespaceOf(t *testing.T) {
	for in, want := range map[string]string{
		"gno.land/r/moul/home":               "moul",
		"gno.land/p/alice/md/v1":             "alice",
		"gno.land/r/moul/x/daily/counter/v0": "moul",
		"gno.land/r/g1abc/foo":               "g1abc",
		"example.com/p/bob/lib/v0":           "bob",
		"gno.land/r":                         "",
		"":                                   "",
	} {
		if got := namespaceOf(in); got != want {
			t.Errorf("namespaceOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- the publish plan, end to end against a fake chain -----------------------

// publishPlan runs `gnopm publish -print` against a fake chain and returns the
// script, the report, and whatever the command decided.
//
// -print, because these tests are about the PLAN: what goes up, in what order,
// and what stops it. The default publishes, which needs a client on PATH and a
// key; publishRun below covers that half with a fake client.
func publishPlan(t *testing.T, root string, f *fakeChain, args ...string) (script, report string, err error) {
	t.Helper()
	var out, errw bytes.Buffer
	full := append([]string{"publish", "-print", "-C", root, "-rpc", f.srv.URL, "-chainid", "test-1"}, args...)
	err = Run(full, &out, &errw)
	return out.String(), errw.String(), err
}

// ranCmd is one invocation a fake client recorded.
type ranCmd struct {
	name string
	args []string
}

// publishRun runs `gnopm publish` with the client replaced, and returns what
// the client was asked to do. fail is the 1-based index of the invocation that
// should fail, or 0 for a run where everything succeeds.
func publishRun(t *testing.T, root string, f *fakeChain, fail int, args ...string) (ran []ranCmd, report string, err error) {
	t.Helper()
	prev := startClient
	t.Cleanup(func() { startClient = prev })
	startClient = func(name string, a []string, _, _ *os.File) error {
		ran = append(ran, ranCmd{name: name, args: append([]string(nil), a...)})
		if fail == len(ran) {
			return fmt.Errorf("fake client refused")
		}
		return nil
	}
	var out, errw bytes.Buffer
	full := append([]string{"publish", "-C", root, "-rpc", f.srv.URL, "-chainid", "test-1"}, args...)
	err = Run(full, &out, &errw)
	return ran, errw.String(), err
}

// chainRepo is three packages in a line: app imports lib imports base.
func chainRepo(t *testing.T) string {
	t.Helper()
	root := newRepo(t)
	addPkg(t, root, "p/moul/base", "gno.land/p/moul/base/v0",
		"package base\n\nfunc B() string { return \"b\" }\n")
	addPkg(t, root, "p/moul/lib", "gno.land/p/moul/lib/v0",
		"package lib\n\nimport \"gno.land/p/moul/base/v0\"\n\nfunc L() string { return base.B() }\n")
	addPkg(t, root, "r/moul/app", "gno.land/r/moul/app/v0",
		"package app\n\nimport \"gno.land/p/moul/lib/v0\"\n\nfunc A() string { return lib.L() }\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	return root
}

// TestPublishFollowsTheImportClosure.
//
// What went wrong: `gnopm publish gno.land/r/moul/x/reaper/v0` answered
// "MISSING dependency, not live: gno.land/p/moul/ulist/v1" for a package
// sitting in the same workspace, a few directories away. The pattern narrowed
// the publish set to the one package it matched, and every import outside that
// set was then judged against the chain alone, so a dependency this very run
// could have published first read as somebody else's problem. The user was
// left to work out the order by hand and run publish once per package, which
// is the job the tool exists to do.
//
// Why no existing test could see it: every publish test ran without a pattern,
// where the set is the whole workspace and the closure is a no-op.
func TestPublishFollowsTheImportClosure(t *testing.T) {
	root := chainRepo(t)
	f := newFakeChain(t)

	script, report, err := publishPlan(t, root, f, "r/moul/app")
	if err != nil {
		t.Fatalf("publish refused a workspace that can publish itself: %v\n%s", err, report)
	}
	for _, m := range []string{"gno.land/p/moul/base/v0", "gno.land/p/moul/lib/v0", "gno.land/r/moul/app/v0"} {
		if !strings.Contains(script, "-pkgpath "+m+" ") {
			t.Fatalf("%s is not in the script:\n%s\n--- report ---\n%s", m, script, report)
		}
	}
	base, lib, app := strings.Index(script, "-pkgpath gno.land/p/moul/base/v0"),
		strings.Index(script, "-pkgpath gno.land/p/moul/lib/v0"),
		strings.Index(script, "-pkgpath gno.land/r/moul/app/v0")
	if !(base < lib && lib < app) {
		t.Fatalf("not in dependency order (base %d, lib %d, app %d):\n%s", base, lib, app, script)
	}
	if strings.Contains(report, "BLOCKED") {
		t.Fatalf("a dependency in the same workspace was reported as a blocker:\n%s", report)
	}
	// The two that no pattern matched are named as what they are, or their
	// presence in the script is a surprise.
	if !strings.Contains(report, "deps     2 package(s) added") {
		t.Errorf("report does not say why base and lib are here:\n%s", report)
	}
	if strings.Count(report, "(dependency)") != 2 {
		t.Errorf("report does not mark the pulled-in packages:\n%s", report)
	}
}

// TestPublishLeavesALivedependencyAlone: the closure must not re-publish what
// is already on chain. addpkg on an occupied path fails, so a script that
// included it would stop at the first command and take the rest with it.
func TestPublishSkipsALiveDependency(t *testing.T) {
	root := chainRepo(t)
	f := newFakeChain(t)
	f.live["gno.land/p/moul/base/v0"] = true

	script, report, err := publishPlan(t, root, f, "r/moul/app")
	if err != nil {
		t.Fatalf("%v\n%s", err, report)
	}
	if strings.Contains(script, "-pkgpath gno.land/p/moul/base/v0") {
		t.Fatalf("the script re-publishes a live package:\n%s", script)
	}
	if !strings.Contains(script, "-pkgpath gno.land/p/moul/lib/v0") ||
		!strings.Contains(script, "-pkgpath gno.land/r/moul/app/v0") {
		t.Fatalf("the script lost what is still absent:\n%s", script)
	}
}

// TestPublishStillBlocksOnADependencyOutsideTheWorkspace: the closure resolves
// against the working tree, and an import that is in neither the tree nor the
// chain is genuinely not ours to publish. Following imports must not turn that
// into a silent pass.
func TestPublishStillBlocksOnADependencyOutsideTheWorkspace(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "r/moul/app", "gno.land/r/moul/app/v0",
		"package app\n\nimport \"gno.land/p/stranger/thing/v0\"\n\nfunc A() { thing.T() }\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	f := newFakeChain(t)

	_, report, err := publishPlan(t, root, f, "app")
	if err == nil {
		t.Fatalf("publish emitted a script for a package whose import is nowhere:\n%s", report)
	}
	if !strings.Contains(report, "not live, and not in this workspace") {
		t.Fatalf("report does not say why it is blocked:\n%s", report)
	}
}

// TestPublishBlocksOnAParkedDependency.
//
// What went wrong: the old check skipped any dependency that was in the plan,
// on the assumption that being in the plan meant being published first. A
// parked package is in the plan and is NOT published by it: its bytes are
// already submitted and waiting on an approver, so the script deliberately
// leaves it alone. Its dependents were then emitted against a path that is not
// live, which is a transaction that fails.
//
// Why no existing test could see it: parked was only ever tested on the
// package being published, never on one it imports. This runs with no pattern
// on purpose, which is the case the old check got wrong: with a pattern it
// refused for the other reason, the closure it did not follow.
func TestPublishBlocksOnAParkedDependency(t *testing.T) {
	root := chainRepo(t)
	f := newFakeChain(t)
	f.parked["gno.land/p/moul/base/v0"] = true

	_, report, err := publishPlan(t, root, f)
	if err == nil {
		t.Fatalf("publish queued a package behind one that is only parked:\n%s", report)
	}
	if !strings.Contains(report, "parked, not live until an approver enables it") {
		t.Fatalf("report does not name the parked dependency:\n%s", report)
	}
}

// TestPublishKeysOffWhatThePatternNamed: a dependency can live in another
// namespace, and the key, the chain and the domain all come from the package
// path. Taking them from the first package in the closure rather than from the
// one the user named would sign somebody else's package with the wrong key.
func TestPublishKeysOffWhatThePatternNamed(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/alice/base", "gno.land/p/alice/base/v0", "package base\n\nfunc B() {}\n")
	addPkg(t, root, "r/moul/app", "gno.land/r/moul/app/v0",
		"package app\n\nimport \"gno.land/p/alice/base/v0\"\n\nfunc A() { base.B() }\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	f := newFakeChain(t)

	_, report, err := publishPlan(t, root, f, "r/moul/app")
	if err != nil {
		t.Fatalf("%v\n%s", err, report)
	}
	if !strings.Contains(report, "key      moul\n") {
		t.Fatalf("key was guessed from a pulled-in dependency, not from what was named:\n%s", report)
	}
}

// TestImportsInReadsOnlyTheImportDeclaration pins the bug that made
// `publish` refuse to emit a script at all: a quoted package path in ordinary
// code or in a doc comment was read as a dependency, and a dependency that is
// neither live nor in the workspace blocks the package and every importer of
// it (gno-contracts#201, and two more found 2026-09-22).
func TestImportsInReadsOnlyTheImportDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{
			"a plain block",
			"package x\n\nimport (\n\t\"gno.land/p/nt/avl/v0\"\n\t\"strings\"\n)\n",
			[]string{"gno.land/p/nt/avl/v0"},
		},
		{
			"an aliased and a single-line import",
			"package x\n\nimport ns \"gno.land/r/a/ns/v0\"\n",
			[]string{"gno.land/r/a/ns/v0"},
		},
		{
			"a doc comment above the package clause",
			"// Usage:\n//\n//\ts.Realm(\"gno.land/r/moul/config\")\npackage x\n\nimport \"gno.land/p/nt/avl/v0\"\n",
			[]string{"gno.land/p/nt/avl/v0"},
		},
		{
			"a doc comment on a function, the kit/ui case",
			"package x\n\nimport \"strings\"\n\n// ActionIn links into gno.land/r/moul/home/v0, which is:\n//\n//\t\"gno.land/r/moul/home/v0\"\nfunc F() {}\n",
			nil,
		},
		{
			"a bare domain in ordinary code",
			"package x\n\nimport \"strings\"\n\nfunc F(p string) string { return strings.TrimPrefix(p, \"gno.land/\") }\n",
			nil,
		},
		{
			"a prefix constant that is not a package path",
			"package x\n\nconst want = \"gno.land/r/moul/config/v\"\n",
			nil,
		},
		{
			"a realm naming itself in a constant",
			"package x\n\nconst realmPath = \"gno.land/r/moul/home\"\n",
			nil,
		},
		{
			"a comment line inside the block",
			"package x\n\nimport (\n\t// see \"gno.land/r/ghost/v0\"\n\t\"gno.land/p/nt/avl/v0\"\n)\n",
			[]string{"gno.land/p/nt/avl/v0"},
		},
		{
			"a block comment before the package clause",
			"/*\nimport (\n\t\"gno.land/r/ghost/v0\"\n)\n*/\npackage x\n\nimport \"gno.land/p/nt/avl/v0\"\n",
			[]string{"gno.land/p/nt/avl/v0"},
		},
		{
			"two import declarations",
			"package x\n\nimport \"gno.land/p/a/v0\"\n\nimport (\n\t\"gno.land/p/b/v0\"\n)\n",
			[]string{"gno.land/p/a/v0", "gno.land/p/b/v0"},
		},
	} {
		got := importsIn(tc.src, "gno.land")
		if len(got) != len(tc.want) {
			t.Errorf("%s: importsIn = %q, want %q", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: importsIn = %q, want %q", tc.name, got, tc.want)
				break
			}
		}
	}
}

// TestImportsInClosesTheBlock covers the shapes gofmt does not write but the
// grammar allows. A block that never closes would put the scanner back to
// reading the whole file, which is the failure mode this replaced.
func TestImportsInClosesTheBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want int
	}{
		{"one-line block", "package x\n\nimport ( \"gno.land/p/a/v0\"; \"gno.land/p/b/v0\" )\n\nconst c = \"gno.land/r/ghost/v0\"\n", 2},
		{"empty one-line block", "package x\n\nimport ()\n\nconst c = \"gno.land/r/ghost/v0\"\n", 0},
		{"closing paren on the last import", "package x\n\nimport (\n\t\"gno.land/p/a/v0\")\n\nconst c = \"gno.land/r/ghost/v0\"\n", 1},
	} {
		if got := importsIn(tc.src, "gno.land"); len(got) != tc.want {
			t.Errorf("%s: importsIn = %q, want %d path(s)", tc.name, got, tc.want)
		}
	}
}

// --- publishing, not printing ------------------------------------------------

// The default has to actually run the commands, in dependency order. Printing
// them was the old default and is now -print; the whole point of the change is
// that nothing has to be copied out of a terminal to land a package.
func TestPublishRunsTheClientInDependencyOrder(t *testing.T) {
	root := chainRepo(t)
	f := newFakeChain(t)

	ran, report, err := publishRun(t, root, f, 0)
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, report)
	}
	if len(ran) != 3 {
		t.Fatalf("ran %d command(s), want 3:\n%+v", len(ran), ran)
	}
	var order []string
	for _, c := range ran {
		if c.name != "gnokey" {
			t.Fatalf("ran %q, want the client", c.name)
		}
		order = append(order, argAfter(t, c.args, "-pkgpath"))
	}
	want := []string{"gno.land/p/moul/base/v0", "gno.land/p/moul/lib/v0", "gno.land/r/moul/app/v0"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("published in order %v, want %v: a dependency has to land first", order, want)
	}
	if !strings.Contains(report, "broadcast 3 package(s)") {
		t.Fatalf("the report must say what is about to be spent before the first prompt:\n%s", report)
	}
}

// -print must run nothing at all. If this ever regresses, a review of a plan
// becomes a deploy of it.
func TestPublishPrintRunsNothing(t *testing.T) {
	root := chainRepo(t)
	f := newFakeChain(t)

	ran, report, err := publishRun(t, root, f, 0, "-print")
	if err != nil {
		t.Fatalf("publish -print: %v\n%s", err, report)
	}
	if len(ran) != 0 {
		t.Fatalf("-print started %d command(s), want none:\n%+v", len(ran), ran)
	}
}

// A failure stops the run. The commands are in dependency order, so continuing
// past one would send a transaction that is certain to be rejected and certain
// to be charged for.
func TestPublishStopsAtTheFirstFailure(t *testing.T) {
	root := chainRepo(t)
	f := newFakeChain(t)

	ran, report, err := publishRun(t, root, f, 2)
	if err == nil {
		t.Fatalf("publish reported success after the client failed:\n%s", report)
	}
	if len(ran) != 2 {
		t.Fatalf("ran %d command(s) after a failure at 2, want 2:\n%+v", len(ran), ran)
	}
	for _, want := range []string{"2 of 3", "re-run"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must say how far it got and what to do next, got: %v", err)
		}
	}
}

// The printed script and the executed command must be the same command. They
// are rendered from one list of arguments precisely so they cannot drift, and
// this is what says so.
func TestPrintAndRunAgreeOnTheArguments(t *testing.T) {
	root := chainRepo(t)
	f := newFakeChain(t)

	script, _, err := publishPlan(t, root, f, "p/moul/base")
	if err != nil {
		t.Fatal(err)
	}
	ran, _, err := publishRun(t, root, f, 0, "p/moul/base")
	if err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 {
		t.Fatalf("ran %d, want 1", len(ran))
	}
	for _, a := range ran[0].args {
		if !strings.Contains(script, a) {
			t.Fatalf("the run used %q and the script does not mention it:\n%s", a, script)
		}
	}
}

// -o is one document and one signature, and it must run the pair too: the
// reason -o exists is one prompt for the whole deploy, which a copy-paste that
// can go stale between the read and the paste takes back.
func TestPublishTxRunsSignThenBroadcast(t *testing.T) {
	root := chainRepo(t)
	f := newFakeChain(t)
	out := filepath.Join(t.TempDir(), "tx.json")

	ran, report, err := publishRun(t, root, f, 0, "-o", out, "-addr", "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5")
	if err != nil {
		t.Fatalf("publish -o: %v\n%s", err, report)
	}
	if len(ran) != 2 {
		t.Fatalf("ran %d command(s), want sign then broadcast:\n%+v", len(ran), ran)
	}
	if ran[0].args[0] != "sign" || ran[1].args[0] != "broadcast" {
		t.Fatalf("wrong order: %q then %q", ran[0].args[0], ran[1].args[0])
	}
	if argAfter(t, ran[0].args, "-tx-path") != out {
		t.Fatalf("sign did not point at the document it wrote: %+v", ran[0].args)
	}
}

// shellQuoteIfNeeded exists so the printed script stays readable. It may only
// leave a value bare when a shell would hand it back unchanged.
func TestShellQuoteIfNeeded(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"-pkgpath", "-pkgpath"},
		{"gno.land/r/moul/blog", "gno.land/r/moul/blog"},
		{"1000ugnot", "1000ugnot"},
		{"", "''"},
		{"two words", "'two words'"},
		{"$(rm -rf /)", `'$(rm -rf /)'`},
		{"it's", `'it'\''s'`},
		{"a;b", "'a;b'"},
	} {
		if got := shellQuoteIfNeeded(tc.in); got != tc.want {
			t.Fatalf("shellQuoteIfNeeded(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func argAfter(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("no %s in %v", flag, args)
	return ""
}
