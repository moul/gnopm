package gnopm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// republishTree writes a one-package workspace and returns its root.
func republishTree(t *testing.T, dir, module string, private bool, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	full := filepath.Join(root, filepath.FromSlash(dir))
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "module = \"" + module + "\"\ngno = \"0.9\"\n"
	if private {
		body += "private = true\n"
	}
	if err := os.WriteFile(filepath.Join(full, "gnomod.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(full, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// onChain seeds the fake chain with a published copy of a package.
func onChain(f *fakeChain, module string, private bool, files map[string]string) {
	f.live[module] = true
	if private {
		f.private[module] = true
	}
	gnomod := "module = \"" + module + "\"\ngno = \"0.9\"\n"
	if private {
		gnomod += "private = true\n"
	}
	f.files[module+"/gnomod.toml"] = gnomod
	for name, content := range files {
		// A case may publish its own gnomod.toml, which is how "the code is
		// identical but the manifest moved" gets expressed.
		f.files[module+"/"+name] = content
	}
}

// TestCheckRepublishWantsBothHalves is the whole contract: a redeploy is
// planned only when the chain would accept it AND there is something to send.
// Each "no" has to name which half failed, because the two have completely
// different fixes: one is "you cannot", the other is "you need not".
func TestCheckRepublishWantsBothHalves(t *testing.T) {
	const module = "gno.land/r/moul/home"
	const dir = "r/moul/home"

	for _, tc := range []struct {
		name         string
		chainPrivate bool
		chainFiles   map[string]string
		localFiles   map[string]string
		wantEligible bool
		wantWhy      string
		wantChanged  []string
	}{
		{
			name:         "private and changed",
			chainPrivate: true,
			chainFiles:   map[string]string{"home.gno": "package home\n"},
			localFiles:   map[string]string{"home.gno": "package home\n\nfunc Render(string) string { return \"\" }\n"},
			wantEligible: true,
			wantChanged:  []string{"home.gno"},
		},
		{
			name:         "private and identical",
			chainPrivate: true,
			chainFiles:   map[string]string{"home.gno": "package home\n"},
			localFiles:   map[string]string{"home.gno": "package home\n"},
			wantWhy:      "identical to the chain's copy",
		},
		{
			// The chain refuses this one, so planning it would produce a
			// transaction that always fails and costs its gas to find out.
			name:         "public on chain",
			chainPrivate: false,
			chainFiles:   map[string]string{"home.gno": "package home\n"},
			localFiles:   map[string]string{"home.gno": "package home\n// changed\n"},
			wantWhy:      "the chain's copy is public",
		},
		{
			name:         "a new file counts as changed",
			chainPrivate: true,
			chainFiles:   map[string]string{"home.gno": "package home\n"},
			localFiles:   map[string]string{"home.gno": "package home\n", "scan.gno": "package home\n"},
			wantEligible: true,
			wantChanged:  []string{"scan.gno (new)"},
		},
		{
			// gnomod.toml travels with the package, so a change to it alone is
			// a real difference. Easy to get wrong by comparing only *.gno,
			// and the failure would be silent: publish would report "identical"
			// about a package whose manifest moved.
			name:         "only the gnomod differs",
			chainPrivate: true,
			chainFiles: map[string]string{
				"home.gno":    "package home\n",
				"gnomod.toml": "module = \"gno.land/r/moul/home\"\ngno = \"0.8\"\nprivate = true\n",
			},
			localFiles:   map[string]string{"home.gno": "package home\n"},
			wantEligible: true,
			wantChanged:  []string{"gnomod.toml"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeChain(t)
			onChain(f, module, tc.chainPrivate, tc.chainFiles)
			root := republishTree(t, dir, module, true, tc.localFiles)
			c := withOverrides(&Chain{}, f.srv.URL, "test-1")

			got, err := checkRepublish(c, root, Package{Dir: dir, Module: module}, false)
			if err != nil {
				t.Fatal(err)
			}
			if got.eligible != tc.wantEligible {
				t.Fatalf("eligible = %v, want %v (why: %q)", got.eligible, tc.wantEligible, got.why)
			}
			if tc.wantWhy != "" && !strings.Contains(got.why, tc.wantWhy) {
				t.Fatalf("why = %q, want it to mention %q", got.why, tc.wantWhy)
			}
			for _, want := range tc.wantChanged {
				if !containsString(got.changed, want) {
					t.Fatalf("changed = %v, want it to include %q", got.changed, want)
				}
			}
		})
	}
}

// TestCheckRepublishRefusesAPreGnomodPackage: a package published before
// gnomod.toml existed carries no flag at all. "No flag" is not "public" as far
// as anything here can prove, but it is equally un-redeployable, and guessing
// otherwise costs a rejected transaction to find out.
//
// The stub SYNTHESIZES a gnomod.toml for anything in f.live, so "live with no
// gnomod" cannot be expressed that way. Seeding only f.files reproduces the
// one thing under test faithfully: the path lists its files, and the query for
// its gnomod.toml comes back an ABCI error, which is exactly what a real chain
// answers for a package stored without one.
func TestCheckRepublishRefusesAPreGnomodPackage(t *testing.T) {
	const module = "gno.land/r/moul/old"
	f := newFakeChain(t)
	f.files[module+"/old.gno"] = "package old\n" // listed, but no gnomod.toml beside it

	root := republishTree(t, "r/moul/old", module, true, map[string]string{"old.gno": "package old\n// new\n"})
	c := withOverrides(&Chain{}, f.srv.URL, "test-1")

	got, err := checkRepublish(c, root, Package{Dir: "r/moul/old", Module: module}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.eligible {
		t.Fatal("planned a redeploy for a package whose chain copy declared no flag")
	}
	if !strings.Contains(got.why, "no gnomod.toml") {
		t.Fatalf("why = %q, want it to name the missing gnomod", got.why)
	}
}

// TestDiffSourceNamesBothDirections. A file that exists only on chain is a
// removal, and the redeploy really does remove it, so it belongs in the list a
// reader approves.
func TestDiffSourceNamesBothDirections(t *testing.T) {
	local := map[string]string{"a.gno": "1", "b.gno": "2", "new.gno": "3"}
	chain := map[string]string{"a.gno": "1", "b.gno": "CHANGED", "gone.gno": "4"}

	got := diffSource(local, chain)
	for _, want := range []string{"b.gno", "new.gno (new)", "gone.gno (removed)"} {
		if !containsString(got, want) {
			t.Errorf("diffSource = %v, want it to include %q", got, want)
		}
	}
	if containsString(got, "a.gno") {
		t.Errorf("diffSource = %v, want it to leave the identical file out", got)
	}
}

// TestPublishesCoversRepublish is the regression test for the bug this change
// nearly shipped.
//
// publishlayers.go carried its own copy of the "is this package going up"
// test, written as `pl.state != StateAbsent`. A republished package is
// StateLive, so it passed every check in publishcmd.go, was counted, was
// reported, and then vanished from layerPlans: the batched path (the DEFAULT
// path) would have written a document with no messages in it and reported
// success. No existing test could see it because no existing test had a plan
// that both publishes and is live, which was an impossible combination until
// this file.
func TestPublishesCoversRepublish(t *testing.T) {
	live := Package{Dir: "r/moul/home", Module: "gno.land/r/moul/home"}
	for _, tc := range []struct {
		name string
		pl   plan
		want bool
	}{
		{"absent", plan{pkg: live, state: StateAbsent}, true},
		{"live, not asked", plan{pkg: live, state: StateLive}, false},
		{"live, republishing", plan{pkg: live, state: StateLive, republish: true}, true},
		{"parked", plan{pkg: live, state: StateParked}, false},
		{"absent but blocked", plan{pkg: live, state: StateAbsent, missing: []depBlock{{"x", "y"}}}, false},
		{"republishing but blocked", plan{pkg: live, state: StateLive, republish: true, missing: []depBlock{{"x", "y"}}}, false},
	} {
		if got := publishes(tc.pl); got != tc.want {
			t.Errorf("%s: publishes = %v, want %v", tc.name, got, tc.want)
		}
	}

	// And the layering itself, which is what actually broke.
	plans := []plan{{pkg: live, state: StateLive, republish: true}}
	layers, err := layerPlans(plans, map[string][]string{live.Module: nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 1 || len(layers[0]) != 1 {
		t.Fatalf("layerPlans dropped the republish: got %v", layers)
	}
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
