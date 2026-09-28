package gnopm

import (
	"strings"
	"testing"
)

// The exact pair observed on mainnet on 2026-09-28: what the repo holds for
// gno.land/r/moul/home, and what vm/qfile returns for the same path minutes
// after it was published from that very file.
const (
	repoGnomod = `module = "gno.land/r/moul/home"
gno = "0.9"

# Redeployable by its creator. See README.md § "Why private".
#
# A private package may be re-added at the same path by the address recorded in
# [addpkg].creator (gno.land/pkg/sdk/vm/keeper.go, checkRedeployPermission), and
# nothing else may import it. A redeploy RE-RUNS init() and resets realm state,
# so content/ is the source of truth and tools/gnohome pushes the slots back.
private = true
`
	chainGnomod = `module = "gno.land/r/moul/home"
gno = "0.9"
private = true

[addpkg]
  creator = "g1manfred47kzduec920z88wfr64ylksmdcedlf5"
  height = 396749
  max_deposit = "17000000ugnot"
`
)

// TestNormalizeGnomodMakesTheRealPairAgree is the defect, verbatim.
//
// Before this, -republish reported gnomod.toml as differing for every package,
// forever, including one redeployed thirty seconds earlier. That is worse than
// noise: the check exists partly to be able to say "nothing to send", and one
// that can never say so pushes somebody into a redeploy that wipes realm state
// for no change at all.
func TestNormalizeGnomodMakesTheRealPairAgree(t *testing.T) {
	if got, want := normalizeGnomod(repoGnomod), normalizeGnomod(chainGnomod); got != want {
		t.Errorf("the repo and the chain still disagree after normalization:\nrepo:  %q\nchain: %q", got, want)
	}
	// And the normalized form is the manifest itself, not an empty string:
	// a normalizer that threw everything away would also make them "agree".
	got := normalizeGnomod(chainGnomod)
	for _, want := range []string{`module = "gno.land/r/moul/home"`, `gno = "0.9"`, "private = true"} {
		if !strings.Contains(got, want) {
			t.Errorf("normalized form lost %q: %q", want, got)
		}
	}
	if strings.Contains(got, "creator") || strings.Contains(got, "[addpkg]") {
		t.Errorf("normalized form still carries the chain-authored table: %q", got)
	}
}

// TestNormalizeGnomodKeepsRealDifferences. The whole point of normalizing one
// file is to keep it honest about the rest of it: a manifest whose gno version
// or private flag actually moved must still register as changed, or -republish
// would refuse to ship a change that matters.
func TestNormalizeGnomodKeepsRealDifferences(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		same bool
	}{
		{"gno version moved", "module = \"m\"\ngno = \"0.9\"\n", "module = \"m\"\ngno = \"0.8\"\n", false},
		{"private flipped", "module = \"m\"\nprivate = true\n", "module = \"m\"\nprivate = false\n", false},
		{"private added", "module = \"m\"\n", "module = \"m\"\nprivate = true\n", false},
		{"module renamed", "module = \"a\"\n", "module = \"b\"\n", false},
		{"only comments differ", "module = \"m\"\n# one\n", "module = \"m\"\n# two\n", true},
		{"only blank lines differ", "module = \"m\"\n\n\n", "module = \"m\"\n", true},
		{"only indentation differs", "module = \"m\"\n  private = true\n", "module = \"m\"\nprivate = true\n", true},
		{"only [addpkg] differs", "module = \"m\"\n[addpkg]\n  height = 1\n", "module = \"m\"\n[addpkg]\n  height = 99\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if same := normalizeGnomod(tc.a) == normalizeGnomod(tc.b); same != tc.same {
				t.Errorf("same = %v, want %v\na: %q\nb: %q",
					same, tc.same, normalizeGnomod(tc.a), normalizeGnomod(tc.b))
			}
		})
	}
}

// TestNormalizeGnomodStopsAddpkgAtTheNextTable. [addpkg] is skipped up to the
// next table header, not to the end of the file. Dropping everything after it
// would silently hide a table a future gno release adds below it.
func TestNormalizeGnomodStopsAddpkgAtTheNextTable(t *testing.T) {
	body := "module = \"m\"\n[addpkg]\n  creator = \"g1x\"\n[other]\n  kept = true\n"
	got := normalizeGnomod(body)
	if !strings.Contains(got, "[other]") || !strings.Contains(got, "kept = true") {
		t.Errorf("a table after [addpkg] was dropped: %q", got)
	}
	if strings.Contains(got, "creator") {
		t.Errorf("[addpkg] survived: %q", got)
	}
}

// TestNormalizeForCompareOnlyTouchesTheManifest. Normalizing source would be
// the opposite mistake: it would hide a real difference in the files that
// actually decide what the realm does.
func TestNormalizeForCompareOnlyTouchesTheManifest(t *testing.T) {
	src := "package home\n\n// a comment\n\nfunc Render(string) string { return \"\" }\n"
	if got := normalizeForCompare("home.gno", src); got != src {
		t.Errorf("source was normalized; comments and blank lines in .gno are real content:\n%q", got)
	}
	if normalizeForCompare("gnomod.toml", repoGnomod) == repoGnomod {
		t.Error("gnomod.toml was not normalized")
	}
}

// TestDiffSourceNoLongerFlagsTheManifest ties it to the caller: the same two
// copies that produced "1 file(s) differ: gnomod.toml" must now produce none.
func TestDiffSourceNoLongerFlagsTheManifest(t *testing.T) {
	local := map[string]string{"gnomod.toml": repoGnomod, "home.gno": "package home\n"}
	chain := map[string]string{"gnomod.toml": chainGnomod, "home.gno": "package home\n"}
	if got := diffSource(local, chain); len(got) != 0 {
		t.Errorf("diffSource = %v, want nothing: the only difference is what the chain writes itself", got)
	}
	// And a real source change still shows.
	chain["home.gno"] = "package home\n// moved\n"
	if got := diffSource(local, chain); len(got) != 1 || got[0] != "home.gno" {
		t.Errorf("diffSource = %v, want exactly home.gno", got)
	}
}
