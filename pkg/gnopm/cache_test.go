package gnopm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cache exists to remove a chain read, so the test that matters is that a
// second run makes fewer calls. Counting states rather than calls would pass
// against a cache that was never consulted.
func TestProbeAnswersFromTheCacheOnASecondRun(t *testing.T) {
	f := newFakeChain(t)
	f.live["gno.land/p/moul/md/v0"] = true
	dir := t.TempDir()
	env := func() *Env { return &Env{Out: os.Stderr, Errw: os.Stderr, CacheDir: dir} }

	p, err := NewProbe(env(), "gno.land/p/moul/md/v0", f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	if s, err := p.State("gno.land/p/moul/md/v0"); err != nil || s != StateLive {
		t.Fatalf("first run: %s, %v", s, err)
	}
	first := f.count()

	// A new probe is a new process as far as the in-memory cache is concerned:
	// only the file survives between them.
	p2, err := NewProbe(env(), "gno.land/p/moul/md/v0", f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	before := f.count()
	if s, err := p2.State("gno.land/p/moul/md/v0"); err != nil || s != StateLive {
		t.Fatalf("second run: %s, %v", s, err)
	}
	if f.count() != before {
		t.Fatalf("the second run made %d chain call(s) for a path it had already seen live", f.count()-before)
	}
	if first == 0 {
		t.Fatal("the first run made no call at all, so the second proves nothing")
	}
	if _, hits, _ := p2.Cache().stats(); hits != 1 {
		t.Fatalf("cache reported %d hit(s), want 1", hits)
	}
}

// Only live is cacheable. Absent is the state of the version you are about to
// publish and parked can still be enabled or rejected, so remembering either
// would make gnopm confidently wrong about the one question it is asked.
func TestOnlyLiveIsRemembered(t *testing.T) {
	f := newFakeChain(t)
	f.live["gno.land/p/moul/md/v0"] = true
	f.parked["gno.land/p/moul/md/v1"] = true
	dir := t.TempDir()

	p, err := NewProbe(&Env{Errw: os.Stderr, CacheDir: dir}, "gno.land/p/moul/md/v0", f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{
		"gno.land/p/moul/md/v0", // live
		"gno.land/p/moul/md/v1", // parked
		"gno.land/p/moul/md/v2", // absent
	} {
		if _, err := p.State(m); err != nil {
			t.Fatal(err)
		}
	}
	body := read(t, filepath.Join(dir, "live", "test-1"))
	got := strings.Fields(body)
	if len(got) != 1 || got[0] != "gno.land/p/moul/md/v0" {
		t.Fatalf("cache file holds %q, want only the live path", body)
	}
}

// A parked path that later goes live must be seen to go live. It is never in
// the file, so the only way this can break is if something starts caching the
// whole probe result rather than the one permanent answer.
func TestAParkedPathIsAskedAgainNextRun(t *testing.T) {
	f := newFakeChain(t)
	f.parked["gno.land/p/moul/md/v0"] = true
	dir := t.TempDir()

	p, err := NewProbe(&Env{Errw: os.Stderr, CacheDir: dir}, "gno.land/p/moul/md/v0", f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := p.State("gno.land/p/moul/md/v0"); s != StateParked {
		t.Fatalf("got %s, want parked", s)
	}

	// The approver enabled it.
	delete(f.parked, "gno.land/p/moul/md/v0")
	f.live["gno.land/p/moul/md/v0"] = true

	p2, err := NewProbe(&Env{Errw: os.Stderr, CacheDir: dir}, "gno.land/p/moul/md/v0", f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := p2.State("gno.land/p/moul/md/v0"); s != StateLive {
		t.Fatalf("got %s, want live: a parked path must be re-asked, not remembered", s)
	}
}

// gnodev reuses chain id "dev" across restarts and each restart is an empty
// chain, so a cache keyed on it would report packages that no longer exist.
// The guard is on the id and not on the address, because a loopback address is
// also how you reach a local node you deliberately keep.
func TestDevChainIsNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, dir, chainID string
		wantFile           bool
	}{
		{name: "a named chain persists", dir: dir, chainID: "gnoland-1", wantFile: true},
		{name: "dev does not", dir: dir, chainID: devChainID},
		{name: "an unnamed chain does not", dir: dir, chainID: ""},
		{name: "no directory does not", dir: "", chainID: "gnoland-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openLiveCache(tc.dir, tc.chainID)
			if got := c.where() != ""; got != tc.wantFile {
				t.Fatalf("persists = %v, want %v", got, tc.wantFile)
			}
			// Either way it still answers, which is what keeps every caller
			// free of a nil check.
			c.learn("gno.land/p/x/y/v0")
			if !c.has("gno.land/p/x/y/v0") {
				t.Fatal("a cache that does not persist must still answer in memory")
			}
		})
	}
}

// The chain id comes off a meta tag on a remote page, so it is not ours to
// trust with a path. A chain calling itself ../../.ssh/authorized_keys must not
// choose where gnopm writes.
func TestChainIDCannotEscapeTheCacheDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"../../escape", "..", "/etc/passwd", "a/b/c"} {
		c := openLiveCache(dir, id)
		if f := c.where(); f != "" && !strings.HasPrefix(filepath.Clean(f), filepath.Join(dir, "live")) {
			t.Fatalf("chain id %q writes to %q, outside %q", id, f, dir)
		}
		c.learn("gno.land/p/x/y/v0")
	}
	// Nothing may have appeared beside the cache directory.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.Name() != "live" {
				t.Fatalf("writing the cache created %q beside it", e.Name())
			}
		}
	}
}

// A cache that cannot be written is still a correct cache. A read-only home
// directory, or a file gnopm has no business in, must cost a chain read and
// never an error.
func TestAnUnwritableCacheIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	// Put a file where the directory has to go, so MkdirAll cannot win.
	if err := os.WriteFile(filepath.Join(dir, "live"), []byte("in the way\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFakeChain(t)
	f.live["gno.land/p/moul/md/v0"] = true
	p, err := NewProbe(&Env{Errw: os.Stderr, CacheDir: dir}, "gno.land/p/moul/md/v0", f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	if s, err := p.State("gno.land/p/moul/md/v0"); err != nil || s != StateLive {
		t.Fatalf("an unwritable cache broke the answer: %s, %v", s, err)
	}
}

// GNOPM_CACHE names the directory, and switches the cache off. Off has to mean
// off: a run with no cache is how you check what the chain actually says.
func TestCacheDirEnvironment(t *testing.T) {
	for _, tc := range []struct {
		set, want string
	}{
		{set: "/tmp/somewhere", want: "/tmp/somewhere"},
		{set: "off", want: ""},
		{set: "OFF", want: ""},
		{set: "none", want: ""},
		{set: "0", want: ""},
	} {
		t.Setenv(cacheEnv, tc.set)
		if got := cacheDir(); got != tc.want {
			t.Errorf("GNOPM_CACHE=%q gives %q, want %q", tc.set, got, tc.want)
		}
	}
	// Unset falls back to the home directory, which is the documented default.
	t.Setenv(cacheEnv, "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	if got, want := cacheDir(), filepath.Join(home, cacheDirName); got != want {
		t.Errorf("default cache dir = %q, want %q", got, want)
	}
}

// The discovery answer is two lines and an mtime. A malformed or absent file
// has to read as "ask the host", never as a half-filled Chain: a wrong rpc
// would point every emitted gnokey command somewhere nobody asked for.
func TestDiscoveryCacheRoundTripAndRejectsRubbish(t *testing.T) {
	dir := t.TempDir()
	if _, _, ok := readDiscovered(dir, "https://gno.land"); ok {
		t.Fatal("an empty cache answered")
	}
	writeDiscovered(dir, "https://gno.land", "https://rpc.gno.land", "gnoland-1")
	rpc, id, ok := readDiscovered(dir, "https://gno.land")
	if !ok || rpc != "https://rpc.gno.land" || id != "gnoland-1" {
		t.Fatalf("readDiscovered = %q, %q, %v", rpc, id, ok)
	}
	for _, junk := range []string{"", "onlyonelines\n", "a\nb\nc\n", "\n\n"} {
		write(t, discoveryFile(dir, "https://gno.land"), junk)
		if _, _, ok := readDiscovered(dir, "https://gno.land"); ok {
			t.Errorf("readDiscovered accepted %q", junk)
		}
	}
}

// -no-cache has to reach the Env, or the flag is decoration. `gnopm env` is
// the one command that reports what gnopm worked out rather than was told, so
// it is also the cheapest place to assert it.
func TestNoCacheFlagAndEnvReportIt(t *testing.T) {
	root := newRepo(t)
	t.Setenv(cacheEnv, filepath.Join(t.TempDir(), "here"))

	out := mustRun(t, root, "env")
	if !strings.Contains(out, "GNOPM_CACHE=") || strings.Contains(out, `GNOPM_CACHE="(off)"`) {
		t.Fatalf("gnopm env does not report the cache:\n%s", out)
	}
	if out := mustRun(t, root, "env", "-no-cache"); !strings.Contains(out, `GNOPM_CACHE="(off)"`) {
		t.Fatalf("-no-cache did not reach the Env:\n%s", out)
	}
}

// -v alone stays the version, because `gnopm -v` has meant that since the
// first release and there is nothing to be verbose about with no command. In
// front of a command it is the verbose flag, which is where anyone would type
// it. --version is unambiguous either way.
func TestDashVIsVersionAloneAndVerboseInFrontOfACommand(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		wantName string
		wantArgs int
	}{
		{args: []string{"-v"}, wantName: "version"},
		{args: []string{"--version"}, wantName: "version"},
		{args: []string{"-v", "status"}, wantName: "status"},
		{args: []string{"-v", "-C", "somewhere", "publish"}, wantName: "publish"},
		{args: []string{"publish"}, wantName: "publish"},
	} {
		name, globals, _, err := hoistGlobals(tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if name != tc.wantName {
			t.Errorf("%v resolves to %q, want %q", tc.args, name, tc.wantName)
		}
		if tc.wantName != "version" && tc.args[0] == "-v" {
			var found bool
			for _, g := range globals {
				if g == "-v" {
					found = true
				}
			}
			if !found {
				t.Errorf("%v: -v was dropped instead of hoisted as a global", tc.args)
			}
		}
	}
}
