package gnopm

import (
	"path/filepath"
	"strings"
	"testing"
)

const treeGnomod = `module = "gno.land/r/moul/zones/v0"
gno = "0.9"

# why this is private
private = true
`

func TestRecordDeploymentIsOneRowPerChain(t *testing.T) {
	onyx := deployment{Chain: "onyx-1", Path: "gno.land/r/g1a/zones/v0", URL: "https://onyx/r/g1a/zones/v0", Revision: "abc1234", At: "2026-10-29T00:00:00Z"}
	got, err := recordDeployment(treeGnomod, onyx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "[[deployment]]") || !strings.Contains(got, `chain = "onyx-1"`) {
		t.Fatalf("no row was written:\n%s", got)
	}
	// What the author wrote survives, comment included.
	if !strings.Contains(got, "# why this is private") || !strings.Contains(got, "private = true") {
		t.Errorf("the rest of the file did not survive:\n%s", got)
	}

	// A second publish to the same chain replaces, it does not accumulate: a
	// package is at one path on one chain, and keeping the old answer keeps a
	// path nobody should read.
	again, err := recordDeployment(got, deployment{Chain: "onyx-1", Path: "gno.land/r/g1a/zones/v1", At: "2026-10-30T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(again, "[[deployment]]"); n != 1 {
		t.Errorf("%d rows for one chain, want 1:\n%s", n, again)
	}
	if !strings.Contains(again, "zones/v1") || strings.Contains(again, "zones/v0\"\n  url") {
		t.Errorf("the row was not replaced:\n%s", again)
	}

	// A different chain is a different row.
	two, err := recordDeployment(again, deployment{Chain: "staging-1", Path: "gno.land/r/g1a/zones/v0"})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(two, "[[deployment]]"); n != 2 {
		t.Errorf("%d rows for two chains, want 2:\n%s", n, two)
	}
	_, rows, err := splitDeployments(two)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Chain != "onyx-1" || rows[1].Chain != "staging-1" {
		t.Errorf("rows = %+v", rows)
	}
}

// The rule from moul/gno-meta#823, and it is the mirror of [source]: the row
// goes in the tree and never into the upload. Unconditionally, on every chain,
// because a row is a statement about some other chain and useless to the one
// being written to.
func TestWithoutDeploymentsStripsEveryRow(t *testing.T) {
	body, err := recordDeployment(treeGnomod, deployment{Chain: "onyx-1", Path: "gno.land/r/g1a/zones/v0"})
	if err != nil {
		t.Fatal(err)
	}
	stripped, err := withoutDeployments(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stripped, "deployment") {
		t.Errorf("a row survived into the upload:\n%s", stripped)
	}
	if !strings.Contains(stripped, "private = true") {
		t.Errorf("stripping took the rest of the file with it:\n%s", stripped)
	}
}

func TestForgetDeploymentDropsOneChain(t *testing.T) {
	body, _ := recordDeployment(treeGnomod, deployment{Chain: "onyx-1", Path: "a"})
	body, _ = recordDeployment(body, deployment{Chain: "staging-1", Path: "b"})
	got, dropped, err := forgetDeployment(body, "onyx-1")
	if err != nil || !dropped {
		t.Fatalf("dropped = %v, err = %v", dropped, err)
	}
	if strings.Contains(got, "onyx-1") || !strings.Contains(got, "staging-1") {
		t.Errorf("wrong row dropped:\n%s", got)
	}
	if _, dropped, _ := forgetDeployment(got, "nowhere-1"); dropped {
		t.Error("a chain with no row reported a drop")
	}
}

// A value this cannot read is an error rather than a guess: the alternative is
// silently losing the record of something that is live on a chain.
func TestSplitDeploymentsRefusesWhatItCannotRead(t *testing.T) {
	_, _, err := splitDeployments("module = \"a\"\n\n[[deployment]]\n  chain = 'onyx-1'\n")
	if err == nil || !strings.Contains(err.Error(), "[[deployment]]") {
		t.Errorf("err = %v, want a complaint", err)
	}
}

// The chain never holds a row, and the tree may, so the comparison that
// decides whether to redeploy must not see one as a difference.
func TestNormalizeGnomodIgnoresDeploymentRows(t *testing.T) {
	body, _ := recordDeployment(treeGnomod, deployment{Chain: "onyx-1", Path: "a"})
	if normalizeGnomod(body) != normalizeGnomod(treeGnomod) {
		t.Errorf("a [[deployment]] row reads as drift:\n%q\nvs\n%q",
			normalizeGnomod(body), normalizeGnomod(treeGnomod))
	}
}

func TestWebOriginFallsBackToTheRPCHost(t *testing.T) {
	for _, tc := range []struct{ host, rpc, override, want string }{
		{"", "https://rpc.onyx.testnets.gno.land", "", "https://onyx.testnets.gno.land"},
		{"", "https://rpc.gno.land:443", "", "https://gno.land"},
		{"https://gno.land", "https://rpc.gno.land", "", "https://gno.land"},
		{"", "http://127.0.0.1:26657", "", ""},
		{"", "http://127.0.0.1:26657", "http://localhost:8888/", "http://localhost:8888"},
	} {
		if got := webOrigin(&Chain{Host: tc.host, RPC: tc.rpc}, tc.override); got != tc.want {
			t.Errorf("webOrigin(%q, %q, %q) = %q, want %q", tc.host, tc.rpc, tc.override, got, tc.want)
		}
	}
	if got := deploymentURL("https://onyx.testnets.gno.land", "gno.land/r/g1a/zones/v0"); got != "https://onyx.testnets.gno.land/r/g1a/zones/v0" {
		t.Errorf("deploymentURL = %q", got)
	}
}

// A row in the tree is stripped from the upload even when nothing moved: the
// upload must never carry one, and the byte count has to follow it or gas is
// sized from a payload that is not the one being sent.
func TestPrepareUploadStripsRowsAndSizesWhatItSends(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "r/moul/zones", "gno.land/r/moul/zones/v0", "package zones\n")
	body, err := recordDeployment(
		readFile(t, filepath.Join(root, "r/moul/zones/gnomod.toml")),
		deployment{Chain: "onyx-1", Path: "gno.land/r/g1a/zones/v0", URL: "https://onyx/r/g1a/zones/v0"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "r/moul/zones/gnomod.toml"), body)

	pl := plan{pkg: Package{Dir: "r/moul/zones", Module: "gno.land/r/moul/zones/v0"}}
	// -source off, so the only edit under test is the stripping.
	if err := pl.prepareUpload(root, newSourceResolver(testEnv(root, nil), true), nil, nil, "gno.land"); err != nil {
		t.Fatal(err)
	}
	up := pl.bodies["gnomod.toml"]
	if strings.Contains(up, "deployment") {
		t.Fatalf("a row went up:\n%s", up)
	}
	total := 0
	for _, f := range pl.files {
		total += f.Size + len(f.Name)
		if f.Name == "gnomod.toml" && f.Size != len(up) {
			t.Errorf("report says gnomod.toml is %d bytes, it uploads %d", f.Size, len(up))
		}
	}
	if pl.bytes != total {
		t.Errorf("bytes = %d, want %d: gas is sized from this", pl.bytes, total)
	}
}
