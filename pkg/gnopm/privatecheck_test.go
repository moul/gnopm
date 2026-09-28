package gnopm

import (
	"io"
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

// sharedCacheEnv is two Envs over ONE cache directory, which is what two
// consecutive gnopm runs on the same machine actually are. A fresh t.TempDir()
// per Env, as probeEnv gives, cannot show that anything was remembered.
func sharedCacheEnv(t *testing.T) (dir string, next func() *Env) {
	t.Helper()
	dir = t.TempDir()
	return dir, func() *Env {
		return &Env{Out: io.Discard, Errw: io.Discard, CacheDir: dir}
	}
}

// TestCheckPrivateAsksTheChainOnce is the regression test for the defect that
// made this cache necessary.
//
// The first cut re-read `<path>/gnomod.toml` for every live package on every
// run and kept nothing. On a 243-package workspace that is 243 extra queries
// per publish, and two whole-workspace runs in quick succession was enough for
// rpc.gno.land to answer this host 403 on everything, /status included.
//
// The answer cannot change (cache.go says why, with the chain's own source), so
// the second run must not touch the chain at all.
func TestCheckPrivateAsksTheChainOnce(t *testing.T) {
	const module = "gno.land/r/moul/faucet/v0"
	f := newFakeChain(t)
	f.live[module] = true // live, public on chain
	root := privateTree(t, "r/moul/faucet", module, true)
	c := withOverrides(&Chain{}, f.srv.URL, "test-1")
	pkgs := []Package{{Dir: "r/moul/faucet", Module: module}}

	_, newEnv := sharedCacheEnv(t)

	first, err := CheckPrivate(newEnv(), c, root, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	cold := f.count()
	if cold == 0 {
		t.Fatal("the first run asked the chain nothing, so this proves nothing")
	}

	second, err := CheckPrivate(newEnv(), c, root, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	if warm := f.count(); warm != cold {
		t.Fatalf("the second run asked the chain %d more time(s); the answer cannot change", warm-cold)
	}
	// And it is the same verdict, not merely a quiet one: a cache that
	// forgets the mismatch is worse than no cache.
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Fatalf("cold and warm disagree:\n cold %+v\n warm %+v", first, second)
	}
}

// TestCheckPrivateRemembersEveryAnswer: all three states have to survive a
// round trip, including "the chain has no gnomod.toml there".
//
// That third one is cacheable for the same reason as the others: a path with no
// gnomod.toml is not private, and AddPackage refuses to redeploy a non-private
// live path at all, so it can never grow one.
func TestCheckPrivateRemembersEveryAnswer(t *testing.T) {
	f := newFakeChain(t)
	// public on chain, private in repo -> a mismatch
	f.live["gno.land/r/moul/a/v0"] = true
	// private on chain, private in repo -> agreement
	f.live["gno.land/r/moul/b/v0"] = true
	f.private["gno.land/r/moul/b/v0"] = true
	// no chain copy at all, so the chain has no gnomod.toml to disagree with,
	// which is the third state and the one a bool could not hold
	// (r/moul/c/v0 is deliberately absent from f.live)

	root := t.TempDir()
	var pkgs []Package
	for _, n := range []string{"a", "b", "c"} {
		dir := filepath.Join(root, "r", "moul", n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "module = \"gno.land/r/moul/" + n + "/v0\"\ngno = \"0.9\"\nprivate = true\n"
		if err := os.WriteFile(filepath.Join(dir, "gnomod.toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, Package{Dir: "r/moul/" + n, Module: "gno.land/r/moul/" + n + "/v0"})
	}
	c := withOverrides(&Chain{}, f.srv.URL, "test-1")
	dir, newEnv := sharedCacheEnv(t)

	cold, err := CheckPrivate(newEnv(), c, root, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	asked := f.count()

	warm, err := CheckPrivate(newEnv(), c, root, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(); n != asked {
		t.Fatalf("the warm run asked %d more time(s)", n-asked)
	}
	if len(cold) != 1 || len(warm) != 1 || cold[0] != warm[0] {
		t.Fatalf("cold %+v, warm %+v: only r/moul/a disagrees, both times", cold, warm)
	}

	// The file holds one line per path, with its state, and nothing else.
	got := map[string]privateState{}
	for _, line := range strings.Split(read(t, filepath.Join(dir, "private", "test-1")), "\n") {
		if st, path, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			got[path] = privateState(st)
		}
	}
	want := map[string]privateState{
		"gno.land/r/moul/a/v0": privatePublic,
		"gno.land/r/moul/b/v0": privatePrivate,
		"gno.land/r/moul/c/v0": privateAbsent,
	}
	for path, st := range want {
		if got[path] != st {
			t.Fatalf("%s cached as %q, want %q (all of %v)", path, got[path], st, got)
		}
	}
}

// TestPrivateCacheSurvivesAGarbledFile: the file is append-only and never
// rewritten, so a write torn by a crash leaves a partial line. That has to read
// as "not asked yet", never as an answer.
func TestPrivateCacheSurvivesAGarbledFile(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "private", "test-1"),
		"private gno.land/r/moul/good/v0\n"+
			"gno.land/r/moul/nospace/v0\n"+ // no state
			"sideways gno.land/r/moul/bogus/v0\n"+ // a state from no version of gnopm
			"public \n"+ // no path
			"public gno.land/r/moul/also/v0\n")

	c := openPrivateCache(dir, "test-1")
	if st, ok := c.lookup("gno.land/r/moul/good/v0"); !ok || st != privatePrivate {
		t.Fatalf("a good line was lost: %q %v", st, ok)
	}
	if st, ok := c.lookup("gno.land/r/moul/also/v0"); !ok || st != privatePublic {
		t.Fatalf("a good line after a bad one was lost: %q %v", st, ok)
	}
	for _, path := range []string{"gno.land/r/moul/nospace/v0", "gno.land/r/moul/bogus/v0"} {
		if _, ok := c.lookup(path); ok {
			t.Fatalf("%s was answered from a garbled line", path)
		}
	}
}

// TestPrivateCacheNeverPersistsDev mirrors the rule liveCache already keeps: a
// gnodev restart wipes the chain and reuses the id, so nothing keyed on it may
// outlive the run.
func TestPrivateCacheNeverPersistsDev(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{devChainID, ""} {
		c := openPrivateCache(dir, id)
		c.learn("gno.land/r/moul/a/v0", privatePrivate)
		if f := c.where(); f != "" {
			t.Fatalf("chain id %q persisted to %s", id, f)
		}
		// It still answers within the run, which is all a dev chain needs.
		if st, ok := c.lookup("gno.land/r/moul/a/v0"); !ok || st != privatePrivate {
			t.Fatalf("chain id %q did not answer from memory", id)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "private")); !os.IsNotExist(err) {
		t.Fatalf("a directory was created for a chain that must not persist: %v", err)
	}
}

// TestCheckPrivateWithNoCacheStillWorks: -no-cache and a machine with no home
// both give an empty CacheDir, and the check has to stay correct there, just
// slower.
func TestCheckPrivateWithNoCacheStillWorks(t *testing.T) {
	const module = "gno.land/r/moul/faucet/v0"
	f := newFakeChain(t)
	f.live[module] = true
	root := privateTree(t, "r/moul/faucet", module, true)
	c := withOverrides(&Chain{}, f.srv.URL, "test-1")
	pkgs := []Package{{Dir: "r/moul/faucet", Module: module}}
	env := func() *Env { return &Env{Out: io.Discard, Errw: io.Discard} } // no CacheDir

	first, err := CheckPrivate(env(), c, root, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	cold := f.count()
	second, err := CheckPrivate(env(), c, root, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	if f.count() == cold {
		t.Fatal("-no-cache reused an answer across runs")
	}
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Fatalf("the verdict changed without a cache: %+v vs %+v", first, second)
	}
}
