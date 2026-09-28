package gnopm

import (
	"strings"
	"testing"
)

// TestDocsOnlyIsNarrow. This rule decides whether a tool declines to ship
// something, so it is pinned in both directions: what it must catch, and
// everything it must NOT quietly swallow.
func TestDocsOnlyIsNarrow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changed []string
		want    bool
	}{
		{"the real case", []string{"README.md"}, true},
		{"several docs", []string{"README.md", "USAGE.md (new)"}, true},
		{"a removed doc", []string{"README.md (removed)"}, true},

		// Anything alongside a doc makes the whole set substantive.
		{"doc plus code", []string{"README.md", "home.gno"}, false},
		{"code alone", []string{"home.gno"}, false},
		{"the manifest", []string{"gnomod.toml"}, false},

		// A test file is inert as far as the VM is concerned, and is still
		// counted as substantive on purpose: see republishclass.go. If this
		// ever flips it should be a decision, not a drift.
		{"a test file", []string{"home_test.gno"}, false},
		{"a filetest", []string{"z0_filetest.gno"}, false},

		// Nothing changed is a different answer with a different message.
		// Collapsing the two would report a package with no differences as one
		// whose differences are not worth shipping.
		{"nothing", nil, false},

		// A name that merely contains .md is not a doc.
		{"md in the middle", []string{"home.md.gno"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := docsOnly(tc.changed); got != tc.want {
				t.Errorf("docsOnly(%v) = %v, want %v", tc.changed, got, tc.want)
			}
		})
	}
}

// TestDiffNameStripsTheAnnotation: a file must be classified by its name, not
// by whether diffSource marked it new or removed.
func TestDiffNameStripsTheAnnotation(t *testing.T) {
	for in, want := range map[string]string{
		"README.md":           "README.md",
		"README.md (new)":     "README.md",
		"README.md (removed)": "README.md",
		"home.gno (new)":      "home.gno",
	} {
		if got := diffName(in); got != want {
			t.Errorf("diffName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCheckRepublishSkipsADocOnlyChange is the defect, end to end, in the shape
// it was found: gno.land/r/moul/home differing only by its README.
//
// The skip must still NAME the file. A reader cannot judge a refusal they
// cannot see, and the earlier version of this reporting printed only the reason.
func TestCheckRepublishSkipsADocOnlyChange(t *testing.T) {
	const module = "gno.land/r/moul/home"
	const dir = "r/moul/home"
	f := newFakeChain(t)
	onChain(f, module, true, map[string]string{
		"home.gno":  "package home\n",
		"README.md": "# home\n\nold prose\n",
	})
	root := republishTree(t, dir, module, true, map[string]string{
		"home.gno":  "package home\n",
		"README.md": "# home\n\nnew prose\n",
	})
	c := withOverrides(&Chain{}, f.srv.URL, "test-1")

	got, err := checkRepublish(c, root, Package{Dir: dir, Module: module}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.eligible {
		t.Fatal("proposed a state-wiping redeploy for a prose change")
	}
	if !strings.Contains(got.why, "documentation only") {
		t.Errorf("why = %q, want it to say why it declined", got.why)
	}
	if !strings.Contains(got.why, "-republish-docs") {
		t.Errorf("why = %q, want it to name the way to override", got.why)
	}
	if len(got.changed) != 1 || got.changed[0] != "README.md" {
		t.Errorf("changed = %v, want the file named so the skip can be judged", got.changed)
	}

	// And the override actually overrides.
	got, err = checkRepublish(c, root, Package{Dir: dir, Module: module}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.eligible {
		t.Fatalf("-republish-docs did not include the package: %q", got.why)
	}
}

// TestCheckRepublishStillShipsCodeBesideDocs. The failure that would matter is
// the opposite one: declining a real change because a README moved with it.
// That is what a commit normally looks like.
func TestCheckRepublishStillShipsCodeBesideDocs(t *testing.T) {
	const module = "gno.land/r/moul/home"
	const dir = "r/moul/home"
	f := newFakeChain(t)
	onChain(f, module, true, map[string]string{
		"home.gno":  "package home\n",
		"README.md": "# home\n",
	})
	root := republishTree(t, dir, module, true, map[string]string{
		"home.gno":  "package home\n\nfunc Render(string) string { return \"\" }\n",
		"README.md": "# home\n\nand a new paragraph\n",
	})
	c := withOverrides(&Chain{}, f.srv.URL, "test-1")

	got, err := checkRepublish(c, root, Package{Dir: dir, Module: module}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !got.eligible {
		t.Fatalf("declined a code change because a doc travelled with it: %q", got.why)
	}
	if len(got.changed) != 2 {
		t.Errorf("changed = %v, want both files reported", got.changed)
	}
}
