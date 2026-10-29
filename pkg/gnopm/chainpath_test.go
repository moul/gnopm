package gnopm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const agentAddr = "g1magld5ptgzaktqcexxq0pgq5lmdf0kz6re8hlv"

func TestAddressPathMovesOnlyTheNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		ok             bool
	}{
		{"a realm", "gno.land/r/moul/zones/v0", "gno.land/r/" + agentAddr + "/zones/v0", true},
		{"a package", "gno.land/p/moul/md/v0", "gno.land/p/" + agentAddr + "/md/v0", true},
		{"a deep path", "gno.land/r/moul/x/upgrade/lazy/v1", "gno.land/r/" + agentAddr + "/x/upgrade/lazy/v1", true},
		{"already under this address", "gno.land/r/" + agentAddr + "/zones/v0", "", false},
		{"someone else's address", "gno.land/r/g1other/zones/v0", "gno.land/r/" + agentAddr + "/zones/v0", true},
		{"not a package path", "gno.land/r/moul", "", false},
		{"not p or r", "gno.land/x/moul/thing/v0", "", false},
	} {
		got, ok := addressPath(tc.in, agentAddr)
		if ok != tc.ok {
			t.Errorf("%s: addressPath ok = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("%s: addressPath = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The rewrite is bounded by the import declaration, and the bound is the whole
// safety argument: a quoted path in ordinary code is a link target, a
// PkgPath() comparison or a prefix constant, and editing one is a textual
// change to program logic. See chainpath.go.
func TestRewriteImportsMovesImportsAndNothingElse(t *testing.T) {
	src := `// Package app does things with gno.land/p/moul/md/v0.
package app

import (
	"strings"

	"gno.land/p/moul/md/v0"
	ufmt "gno.land/p/nt/ufmt"
)

const home = "gno.land/r/moul/home/v0"

func Render(_ string) string {
	if strings.HasPrefix(home, "gno.land/r/moul/") {
		return md.H1("x") + ufmt.Sprintf("%s", home)
	}
	return ""
}
`
	to := func(p string) string {
		switch p {
		case "gno.land/p/moul/md/v0":
			return "gno.land/p/" + agentAddr + "/md/v0"
		case "gno.land/r/moul/home/v0":
			return "gno.land/r/" + agentAddr + "/home/v0"
		}
		return p
	}
	got := rewriteImports(src, "gno.land", to)

	if !strings.Contains(got, `"gno.land/p/`+agentAddr+`/md/v0"`) {
		t.Errorf("the import was not moved:\n%s", got)
	}
	if !strings.Contains(got, `ufmt "gno.land/p/nt/ufmt"`) {
		t.Errorf("an import outside this workspace was touched; it exists on the chain under its real path:\n%s", got)
	}
	if !strings.Contains(got, `const home = "gno.land/r/moul/home/v0"`) {
		t.Errorf("a path in a string literal was rewritten; that is program logic, not an import:\n%s", got)
	}
	if !strings.Contains(got, `// Package app does things with gno.land/p/moul/md/v0.`) {
		t.Errorf("the doc comment was rewritten:\n%s", got)
	}
	if !strings.Contains(got, `strings.HasPrefix(home, "gno.land/r/moul/")`) {
		t.Errorf("a prefix constant was rewritten:\n%s", got)
	}

	// And the ones it did not touch are named, rather than discovered on chain.
	outside := outsideImports(src, "gno.land", func(p string) bool { return to(p) != p })
	if len(outside) != 1 || outside[0] != "gno.land/r/moul/home/v0" {
		t.Errorf("outsideImports = %q, want the one path named outside an import", outside)
	}
}

func TestRewriteImportsLeavesAFileWithNothingToMove(t *testing.T) {
	src := "package a\n\nimport \"gno.land/p/nt/avl\"\n\nfunc A() {}\n"
	if got := rewriteImports(src, "gno.land", func(p string) string { return p }); got != src {
		t.Errorf("a file with nothing to move was rewritten:\n%s", got)
	}
}

func TestWithModuleRewritesOnlyTheModuleLine(t *testing.T) {
	in := "module = \"gno.land/r/moul/zones/v0\"\ngno = \"0.9\"\nprivate = true\n"
	got := withModule(in, "gno.land/r/"+agentAddr+"/zones/v0")
	want := "module = \"gno.land/r/" + agentAddr + "/zones/v0\"\ngno = \"0.9\"\nprivate = true\n"
	if got != want {
		t.Errorf("withModule =\n%s\nwant\n%s", got, want)
	}
	if got := withModule("gno = \"0.9\"\n", "x"); got != "gno = \"0.9\"\n" {
		t.Errorf("a file with no module line was changed: %q", got)
	}
}

// -path auto asks the chain's own verifier, and asks it once per NAMESPACE.
// Once per package would be 320 round trips on this workspace for an answer
// that is the same every time.
func TestChainPathsAsksTheChainOncePerNamespace(t *testing.T) {
	f := newFakeChain(t)
	f.owners["alice"] = "g1alice"
	e := probeEnv(t)
	c := &Chain{RPC: f.srv.URL, ID: "test-1"}

	mods := []string{
		"gno.land/p/moul/md/v0", "gno.land/p/moul/md/v1", "gno.land/r/moul/home/v0",
		"gno.land/p/alice/x/v0",
	}
	pm, err := chainPaths(e, c, pathAuto, agentAddr, mods)
	if err != nil {
		t.Fatal(err)
	}
	if f.evals != 2 {
		t.Errorf("the chain was asked %d times, want once per namespace (2)", f.evals)
	}
	if pm.count() != 4 {
		t.Fatalf("%d package(s) moved, want all 4: this signer owns neither namespace", pm.count())
	}
	if got := pm.Upload("gno.land/r/moul/home/v0"); got != "gno.land/r/"+agentAddr+"/home/v0" {
		t.Errorf("Upload = %q", got)
	}
	if got := pm.Upload("gno.land/p/nt/avl/v0"); got != "gno.land/p/nt/avl/v0" {
		t.Errorf("a path outside the workspace was moved: %q", got)
	}
}

// The owner's own key on the chain that registered the name: nothing moves, and
// nothing downstream can tell this code is there. This is mainnet.
func TestChainPathsLeavesTheOwnerAlone(t *testing.T) {
	f := newFakeChain(t)
	f.owners["moul"] = "g1manfred47kzduec920z88wfr64ylksmdcedlf5"
	e := probeEnv(t)
	c := &Chain{RPC: f.srv.URL, ID: "test-1"}
	pm, err := chainPaths(e, c, pathAuto, "g1manfred47kzduec920z88wfr64ylksmdcedlf5",
		[]string{"gno.land/p/moul/md/v0", "gno.land/r/moul/home/v0"})
	if err != nil {
		t.Fatal(err)
	}
	if pm.moved() {
		t.Errorf("the namespace owner's publish moved %d path(s); it must move none", pm.count())
	}
}

// A chain that will not answer is left alone, and the two errors are not
// symmetric: publishing at the canonical path when the signer is not
// authorized wastes a transaction the chain refuses, while publishing at an
// address path when they WERE authorized puts the package at a path nobody
// asked for and that cannot be taken back.
func TestChainPathsLeavesThePathAloneWhenTheChainWillNotSay(t *testing.T) {
	f := newFakeChain(t)
	f.noNames = true
	var sb strings.Builder
	e := &Env{Out: &sb, Errw: &sb, CacheDir: t.TempDir()}
	c := &Chain{RPC: f.srv.URL, ID: "test-1"}
	pm, err := chainPaths(e, c, pathAuto, agentAddr, []string{"gno.land/r/moul/home/v0"})
	if err != nil {
		t.Fatal(err)
	}
	if pm.moved() {
		t.Error("a chain that could not answer moved a path anyway")
	}
	if !strings.Contains(sb.String(), "could not say whether") {
		t.Errorf("the run was silent about it:\n%s", sb.String())
	}
}

func TestChainPathsRejectsAnUnknownMode(t *testing.T) {
	_, err := chainPaths(probeEnv(t), &Chain{ID: "x"}, "sideways", agentAddr, nil)
	if err == nil || !strings.Contains(err.Error(), "-path") {
		t.Errorf("err = %v, want a complaint about -path", err)
	}
}

// End to end, and the case the whole file exists for: a package goes up under
// the signer's address AND the import of its workspace dependency goes with
// it. Without the second half the deploy is broken, because the chain has
// nothing at the path the import names.
func TestPublishMovesAPathAndItsImports(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/base", "gno.land/p/moul/base/v0",
		"package base\n\nfunc B() string { return \"b\" }\n")
	write(t, filepath.Join(root, "r/moul/app/gnomod.toml"),
		"module = \"gno.land/r/moul/app/v0\"\ngno = \"0.9\"\n")
	write(t, filepath.Join(root, "r/moul/app/app.gno"),
		"package app\n\nimport (\n\t\"gno.land/p/moul/base/v0\"\n\t\"gno.land/p/nt/avl\"\n)\n\nvar _ = avl.Tree{}\n\nfunc A() string { return base.B() }\n")
	commit(t, root, "init")
	mustRun(t, root, "sync")

	f := newFakeChain(t)
	f.live["gno.land/p/nt/avl"] = true
	out := filepath.Join(t.TempDir(), "tx.json")
	mustRun(t, root, "publish", "-print", "-rpc", f.srv.URL, "-chainid", "test-1",
		"-key", "agent", "-addr", agentAddr, "-o", out)

	// Two layers, so two documents: base goes up before what imports it.
	doc := readFile(t, strings.Replace(out, ".json", ".1.json", 1)) +
		readFile(t, strings.Replace(out, ".json", ".2.json", 1))
	for _, want := range []string{
		`"path": "gno.land/p/` + agentAddr + `/base/v0"`,
		`"path": "gno.land/r/` + agentAddr + `/app/v0"`,
		`\"gno.land/p/` + agentAddr + `/base/v0\"`,
		`module = \"gno.land/p/` + agentAddr + `/base/v0\"`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the transaction document is missing %s\n%s", want, doc)
		}
	}
	if strings.Contains(doc, `\"gno.land/p/moul/base/v0\"`) {
		t.Errorf("an import still names the canonical path, which does not exist on this chain:\n%s", doc)
	}
	if !strings.Contains(doc, `\"gno.land/p/nt/avl\"`) {
		t.Errorf("an import outside this workspace was moved; it is live under its real path:\n%s", doc)
	}
	// The tree is never touched by the rewrite.
	if got := readFile(t, filepath.Join(root, "r/moul/app/app.gno")); !strings.Contains(got, `"gno.land/p/moul/base/v0"`) {
		t.Errorf("publish rewrote the working tree:\n%s", got)
	}
}

// gnokey reads the directory itself under -pkgdir, so an unbatched run cannot
// be handed rewritten bytes. Refusing beats sending a package whose
// dependencies do not exist on the chain it is going to.
func TestPublishRefusesOneTxPerPackageWhereThePathMoves(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/base", "gno.land/p/moul/base/v0", "package base\n")
	commit(t, root, "init")
	mustRun(t, root, "sync")

	f := newFakeChain(t)
	err := Run([]string{"publish", "-C", root, "-print", "-rpc", f.srv.URL, "-chainid", "test-1",
		"-key", "agent", "-addr", agentAddr, "-one-tx-per-package"}, os.Stderr, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "-one-tx-per-package cannot be used where the path moves") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// -path canonical is the way back to the old behaviour, for someone who wants
// the chain to refuse the transaction rather than have gnopm move it.
func TestPublishCanonicalModeMovesNothing(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/base", "gno.land/p/moul/base/v0", "package base\n")
	commit(t, root, "init")
	mustRun(t, root, "sync")

	f := newFakeChain(t)
	out := filepath.Join(t.TempDir(), "tx.json")
	mustRun(t, root, "publish", "-print", "-rpc", f.srv.URL, "-chainid", "test-1",
		"-key", "agent", "-addr", agentAddr, "-path", "canonical", "-o", out)
	if doc := readFile(t, out); !strings.Contains(doc, `"path": "gno.land/p/moul/base/v0"`) {
		t.Errorf("-path canonical moved a path:\n%s", doc)
	}
	if f.evals != 0 {
		t.Errorf("-path canonical asked the chain %d question(s); it should ask none", f.evals)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The drift question, and the answer this PR implements for the republish
// comparison: what is compared is what WOULD be sent, against what the chain
// holds at the path it was sent to. Without both halves every package
// rehearsed under an address path reads as drifted forever, which is a
// reported failure for a deploy that is exactly right.
func TestRepublishComparesTheRewrittenCopyAtTheMovedPath(t *testing.T) {
	root := newRepo(t)
	write(t, filepath.Join(root, "r/moul/app/gnomod.toml"),
		"module = \"gno.land/r/moul/app/v0\"\ngno = \"0.9\"\nprivate = true\n")
	write(t, filepath.Join(root, "r/moul/app/app.gno"),
		"package app\n\nimport \"gno.land/p/moul/base/v0\"\n\nfunc A() string { return base.B() }\n")
	commit(t, root, "init")

	moved := "gno.land/r/" + agentAddr + "/app/v0"
	pm := &pathMap{to: map[string]string{
		"gno.land/r/moul/app/v0":  moved,
		"gno.land/p/moul/base/v0": "gno.land/p/" + agentAddr + "/base/v0",
	}}
	pl := plan{pkg: Package{Dir: "r/moul/app", Module: "gno.land/r/moul/app/v0"}, upload: moved}
	if err := pl.prepareUpload(root, newSourceResolver(testEnv(root, nil), true), nil, pm, "gno.land"); err != nil {
		t.Fatal(err)
	}

	// The chain holds exactly what this run would send, at the moved path.
	f := newFakeChain(t)
	f.live[moved] = true
	f.private[moved] = true
	f.files[moved+"/app.gno"] = pl.bodies["app.gno"]
	f.files[moved+"/gnomod.toml"] = pl.bodies["gnomod.toml"]

	s := &republishScanner{chain: &Chain{RPC: f.srv.URL, ID: "test-1"}, root: root}
	got, err := s.check(pl.pkg, pl.upload, pl.bodies)
	if err != nil {
		t.Fatal(err)
	}
	if got.eligible {
		t.Errorf("a rehearsal under a moved path reads as drifted: %q", got.changed)
	}
	if !strings.Contains(got.why, "identical") {
		t.Errorf("why = %q, want it to say the copies match", got.why)
	}

	// And a real source change still shows up, which is the half that must not
	// be traded away for the half above.
	write(t, filepath.Join(root, "r/moul/app/app.gno"),
		"package app\n\nimport \"gno.land/p/moul/base/v0\"\n\nfunc A() string { return base.B() + \"!\" }\n")
	pl2 := plan{pkg: pl.pkg, upload: moved}
	if err := pl2.prepareUpload(root, newSourceResolver(testEnv(root, nil), true), nil, pm, "gno.land"); err != nil {
		t.Fatal(err)
	}
	got2, err := s.check(pl2.pkg, pl2.upload, pl2.bodies)
	if err != nil {
		t.Fatal(err)
	}
	if !got2.eligible || len(got2.changed) != 1 || got2.changed[0] != "app.gno" {
		t.Errorf("a real change was missed: eligible=%v changed=%q", got2.eligible, got2.changed)
	}
}

// verify -deployed holds no key, so it cannot work the mapping out the way
// publish does. The rows ARE the mapping, which is the third reason they exist.
func TestVerifyDeployedReadsTheDeploymentRow(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "r/moul/app", "gno.land/r/moul/app/v0", "package app\n\nfunc A() {}\n")
	moved := "gno.land/r/" + agentAddr + "/app/v0"
	body, err := recordDeployment(
		readFile(t, filepath.Join(root, "r/moul/app/gnomod.toml")),
		deployment{Chain: "test-1", Path: moved})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "r/moul/app/gnomod.toml"), body)
	commit(t, root, "init")
	mustRun(t, root, "sync")

	f := newFakeChain(t)
	f.live[moved] = true
	f.files[moved+"/app.gno"] = "package app\n\nfunc A() {}\n"
	// What the chain stores: the module line is the deploy path, and it has
	// stamped its own [addpkg] table over what it was sent.
	f.files[moved+"/gnomod.toml"] = "module = \"" + moved + "\"\ngno = \"0.9\"\n\n[addpkg]\n  creator = \"" + agentAddr + "\"\n  height = 9\n"

	out := mustRun(t, root, "verify", "-deployed", "-rpc", f.srv.URL, "-chainid", "test-1")
	if !strings.Contains(out, "1 live, 1 match") {
		t.Errorf("the recorded path was not read back as a match:\n%s", out)
	}
	if !strings.Contains(out, "deployed under a different path") {
		t.Errorf("the run was silent about the mapping it used:\n%s", out)
	}
}
