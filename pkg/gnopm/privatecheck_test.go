package gnopm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGnomodFlagDoesNotReadItsOwnDocumentation is the trap that produced a
// false alarm before this function existed.
//
// The obvious implementation is a substring or an unanchored regexp, and both
// are wrong for the same reason: a package that stays public documents WHY in
// a comment, and the sentence naturally contains the thing it is explaining.
// A real gno-contracts gnomod says "As private = true the first CreatePool
// panics"; read without skipping comments, that reads as a declaration. Three
// of four reported mismatches on 2026-09-23 were sentences.
//
// No existing test could see it: parseGnomod was only ever asked about
// `module` and `ignore`, and nothing writes prose about either.
func TestGnomodFlagDoesNotReadItsOwnDocumentation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"declared", "module = \"m\"\ngno = \"0.9\"\nprivate = true\n", true},
		{"absent", "module = \"m\"\ngno = \"0.9\"\n", false},
		{"explicit false", "module = \"m\"\nprivate = false\n", false},
		{
			"explained in a comment",
			"module = \"m\"\ngno = \"0.9\"\n\n# public: As private = true the first CreatePool panics\n",
			false,
		},
		{
			"trailing comment on a false",
			"module = \"m\"\nprivate = false # was true until v2\n",
			false,
		},
		{"trailing comment on a true", "module = \"m\"\nprivate = true # deliberate\n", true},
		{
			// `private` under a table is a different key. [addpkg] is written
			// by the chain itself, so this is not hypothetical.
			"under a table header",
			"module = \"m\"\n\n[addpkg]\n  private = true\n",
			false,
		},
		{"indented", "module = \"m\"\n  private = true\n", true},
	} {
		if got := gnomodFlag([]byte(tc.body), "private"); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// privateTree writes a one-package workspace and returns its root, so a test
// can state what the REPO believes independently of what the chain says.
func privateTree(t *testing.T, dir, module string, private bool) string {
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
	return root
}

// TestCheckPrivateCatchesTheFaucetShape is the defect this file exists for.
//
// gno.land/r/moul/faucet/v0 was deployed public and had `private = true` added
// to its gnomod two minutes later. The flag binds at submit time, so the repo
// has advertised a redeployable realm ever since and the chain has never held
// one. Nothing noticed for three days, because every tool that reads the flag
// reads it from the repo and every tool that reads the chain reads only
// whether the path resolves.
func TestCheckPrivateCatchesTheFaucetShape(t *testing.T) {
	const module = "gno.land/r/moul/faucet/v0"
	f := newFakeChain(t)
	f.live[module] = true // live, and NOT in f.private: the chain's copy is public

	root := privateTree(t, "r/moul/faucet", module, true) // the repo says private
	c := withOverrides(&Chain{}, f.srv.URL, "test-1")

	got, err := CheckPrivate(probeEnv(t), c, root, []Package{{Dir: "r/moul/faucet", Module: module}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d mismatch(es), want 1: %+v", len(got), got)
	}
	if got[0].module != module || !got[0].inRepo || got[0].onChain {
		t.Fatalf("mismatch describes the wrong direction: %+v", got[0])
	}
	if want := "frozen public"; !strings.Contains(got[0].why(), want) {
		t.Fatalf("why() does not say what it costs: %q", got[0].why())
	}
}

// TestCheckPrivateCatchesTheReverse: the chain holds a private package and the
// repo forgot to say so. Less costly than the faucet shape but the same lie,
// and it hides a redeploy that is actually available.
func TestCheckPrivateCatchesTheReverse(t *testing.T) {
	const module = "gno.land/r/moul/home"
	f := newFakeChain(t)
	f.live[module] = true
	f.private[module] = true

	root := privateTree(t, "r/moul/home", module, false)
	c := withOverrides(&Chain{}, f.srv.URL, "test-1")

	got, err := CheckPrivate(probeEnv(t), c, root, []Package{{Dir: "r/moul/home", Module: module}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].inRepo || !got[0].onChain {
		t.Fatalf("got %+v, want one mismatch with onChain set", got)
	}
}

// TestCheckPrivateIsSilentWhenTheyAgree, in both directions. 55 of the 58
// packages declaring the flag in gno-contracts agree with the chain, so a
// check that cried on those would be turned off within a day.
func TestCheckPrivateIsSilentWhenTheyAgree(t *testing.T) {
	f := newFakeChain(t)
	f.live["gno.land/r/moul/a/v0"] = true
	f.live["gno.land/r/moul/b/v0"] = true
	f.private["gno.land/r/moul/a/v0"] = true

	root := t.TempDir()
	for _, p := range []struct {
		dir, module string
		private     bool
	}{
		{"r/moul/a", "gno.land/r/moul/a/v0", true},
		{"r/moul/b", "gno.land/r/moul/b/v0", false},
	} {
		full := filepath.Join(root, filepath.FromSlash(p.dir))
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "module = \"" + p.module + "\"\ngno = \"0.9\"\n"
		if p.private {
			body += "private = true\n"
		}
		if err := os.WriteFile(filepath.Join(full, "gnomod.toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := withOverrides(&Chain{}, f.srv.URL, "test-1")
	got, err := CheckPrivate(probeEnv(t), c, root, []Package{
		{Dir: "r/moul/a", Module: "gno.land/r/moul/a/v0"},
		{Dir: "r/moul/b", Module: "gno.land/r/moul/b/v0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("agreeing packages reported as mismatches: %+v", got)
	}
}

// TestCheckPrivateDoesNotLetAnUnreachableChainAgree is CONTRIBUTING's seventh
// claim applied here. A node that does not answer must not read as a node that
// said "no flag": that would turn every mismatch into silence exactly when the
// operator is least able to notice, and publish would then proceed.
func TestCheckPrivateDoesNotLetAnUnreachableChainAgree(t *testing.T) {
	const module = "gno.land/r/moul/faucet/v0"
	f := newFakeChain(t)
	f.live[module] = true
	f.down = true

	root := privateTree(t, "r/moul/faucet", module, true)
	c := withOverrides(&Chain{}, f.srv.URL, "test-1")

	if _, err := CheckPrivate(probeEnv(t), c, root, []Package{{Dir: "r/moul/faucet", Module: module}}); err == nil {
		t.Fatal("an unreachable chain reported agreement instead of an error")
	}
}

// TestCheckPrivateSkipsAPackageTheChainHasNoGnomodFor: a path that does not
// resolve has nothing to compare, and saying "the chain declares no private"
// about it would report every absent package as a mismatch.
func TestCheckPrivateSkipsAPackageTheChainHasNoGnomodFor(t *testing.T) {
	const module = "gno.land/r/moul/new/v0"
	f := newFakeChain(t) // nothing live

	root := privateTree(t, "r/moul/new", module, true)
	c := withOverrides(&Chain{}, f.srv.URL, "test-1")

	got, err := CheckPrivate(probeEnv(t), c, root, []Package{{Dir: "r/moul/new", Module: module}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a package with no chain copy was reported: %+v", got)
	}
}
