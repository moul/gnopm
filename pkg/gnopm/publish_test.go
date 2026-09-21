package gnopm

import (
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
		10_000_000: "100000ugnot",
		53_182_800: "531828ugnot",
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
	if r := float64(fee) / float64(gas); r < 0.009 || r > 0.011 {
		t.Errorf("ratio %.4f ugnot/gas, want ~0.01, ten times the observed floor", r)
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
	c, err := DiscoverChain("nodomain/whatever", "http://127.0.0.1:26657", "dev")
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
