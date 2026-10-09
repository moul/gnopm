package gnopm

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// What no existing test could see: every republish test compares exactly one
// package, where the round trips are invisible and the caches have nothing to
// carry between runs. The cost this file is about only appears at workspace
// size, where moul/gno-contracts measured 1269s end to end for 282 live
// packages and the comparison itself was never the slow part.

// scannerFor builds a scanner over a fake chain, with caches that live in the
// test's own directory so nothing here touches a developer's ~/.gnopm.
func scannerFor(t *testing.T, f *fakeChain, root string) *republishScanner {
	t.Helper()
	dir := t.TempDir()
	return &republishScanner{
		chain:   &Chain{RPC: f.srv.URL, ID: "test-1"},
		root:    root,
		private: openPrivateCache(dir, "test-1"),
		source:  openSourceCache(dir, "test-1"),
	}
}

// A public package can never be republished, and that is permanent. Asking the
// chain again on every run is 227 of the 282 reads moul/gno-contracts spends,
// for an answer that is frozen at first publish (cache.go says why, with the
// chain's source to back it).
func TestScannerAsksOnceWhetherAPathIsPublic(t *testing.T) {
	const module = "gno.land/r/moul/home"
	f := newFakeChain(t)
	onChain(f, module, false, map[string]string{"home.gno": "package home\n"})
	root := republishTree(t, "r/moul/home", module, false, map[string]string{"home.gno": "package home\n// changed\n"})
	s := scannerFor(t, f, root)
	pkg := Package{Dir: "r/moul/home", Module: module}

	first, err := s.check(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if first.eligible || !strings.Contains(first.why, "public") {
		t.Fatalf("check = %+v, want a refusal naming the public copy", first)
	}
	after := f.count()

	second, err := s.check(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if second.why != first.why {
		t.Fatalf("second check said %q, want the same as the first", second.why)
	}
	if n := f.count() - after; n != 0 {
		t.Fatalf("the second check made %d chain read(s), want 0: `private` cannot change", n)
	}
}

// vm/qstorage is one read that moves when a package's bytes on chain move, so
// an unchanged figure means the copy already on disk is still the chain's.
// Without this, confirming a package is unchanged costs its whole source every
// run: on gno-contracts, 76 packages at one read per file.
func TestScannerReReadsTheSourceOnlyWhenStorageMoved(t *testing.T) {
	const module = "gno.land/r/moul/home"
	files := map[string]string{"home.gno": "package home\n"}
	f := newFakeChain(t)
	onChain(f, module, true, files)
	f.storage[module] = "storage: 100, deposit: 10000"
	root := republishTree(t, "r/moul/home", module, true, files)
	s := scannerFor(t, f, root)
	pkg := Package{Dir: "r/moul/home", Module: module}

	if _, err := s.check(pkg); err != nil {
		t.Fatal(err)
	}
	first := f.count()

	// Unchanged storage: one read for the token, and nothing else.
	if _, err := s.check(pkg); err != nil {
		t.Fatal(err)
	}
	if n := f.count() - first; n != 1 {
		t.Fatalf("an unchanged package cost %d read(s), want 1 (the storage token alone)", n)
	}
	if _, hits, _ := s.source.stats(); hits != 1 {
		t.Fatalf("source cache hits = %d, want 1", hits)
	}

	// Moved storage: the cached copy is not the chain's any more, so the
	// files come back. Reported as changed, which is the answer that matters.
	f.mu.Lock()
	f.storage[module] = "storage: 200, deposit: 20000"
	f.files[module+"/home.gno"] = "package home\n// somebody redeployed\n"
	f.mu.Unlock()
	before := f.count()
	got, err := s.check(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count() - before; n < 2 {
		t.Fatalf("a moved storage figure cost %d read(s), want the source re-read", n)
	}
	if !got.eligible || len(got.changed) == 0 {
		t.Fatalf("check = %+v, want the redeploy seen", got)
	}
}

// A chain with no vm/qstorage must still work, and must not pay a wasted read
// per package to keep finding that out.
func TestScannerStopsAskingAChainThatHasNoStorageQuery(t *testing.T) {
	const module = "gno.land/r/moul/home"
	files := map[string]string{"home.gno": "package home\n"}
	f := newFakeChain(t)
	onChain(f, module, true, files)
	root := republishTree(t, "r/moul/home", module, true, files)
	s := scannerFor(t, f, root)
	pkg := Package{Dir: "r/moul/home", Module: module}

	if _, err := s.check(pkg); err != nil {
		t.Fatal(err)
	}
	if !s.storageOff {
		t.Fatal("the scanner kept vm/qstorage on after the chain refused it")
	}
	got, err := s.check(pkg)
	if err != nil {
		t.Fatal(err)
	}
	if got.eligible {
		t.Fatalf("check = %+v, want identical with no chain storage to lean on", got)
	}
}

// The batch must carry a transport failure out rather than let the workers
// around it fill in verdicts, exactly as Probe.Warm does: an unreachable node
// read as "nothing drifted" is the worst answer this command can give.
func TestScanReportsATransportFailureRatherThanNoDrift(t *testing.T) {
	f := newFakeChain(t)
	var targets []*plan
	for i := 0; i < 12; i++ {
		m := fmt.Sprintf("gno.land/r/moul/pkg%02d/v0", i)
		onChain(f, m, true, map[string]string{"x.gno": "package x\n"})
		targets = append(targets, &plan{pkg: Package{Dir: "r/moul/x", Module: m}})
	}
	s := scannerFor(t, f, t.TempDir())
	// The fake is "down" with a 502, which chain.go retries briefly. Stubbed
	// so the suite does not wait out a real backoff to learn the same thing.
	retrySleep = func(time.Duration) {}
	defer func() { retrySleep = time.Sleep }()
	f.down = true

	err := s.scan(targets)
	if err == nil {
		t.Fatal("scan read an unreachable chain as a workspace with nothing to republish")
	}
	if answered(err) {
		t.Fatalf("a transport failure came back as a chain answer: %v", err)
	}
	for _, pl := range targets {
		if pl.republish || pl.skipped != "" {
			t.Fatalf("a failed scan left a verdict on %s", pl.pkg.Module)
		}
	}
}

// The scan is a batch, so it has to be safe to run wide. Counting the peak
// concurrency is the only way to tell a parallel scan from a serial loop that
// happens to be fast on a fake.
func TestScanRunsInParallel(t *testing.T) {
	f := newFakeChain(t)
	var targets []*plan
	for i := 0; i < scanConcurrency*2; i++ {
		m := fmt.Sprintf("gno.land/r/moul/pkg%02d/v0", i)
		onChain(f, m, false, map[string]string{"x.gno": "package x\n"})
		targets = append(targets, &plan{pkg: Package{Dir: "r/moul/x", Module: m}})
	}
	var inFlight, peak int32
	// Held open briefly, because overlap is only observable while a request
	// is still in flight: a fake that answers instantly makes a serial loop
	// and a wide batch look identical.
	f.onQuery = func() {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
	}
	s := scannerFor(t, f, t.TempDir())
	if err := s.scan(targets); err != nil {
		t.Fatal(err)
	}
	if peak < 2 {
		t.Fatalf("peak concurrency %d: the scan ran serially", peak)
	}
	for _, pl := range targets {
		if pl.skipped == "" {
			t.Fatalf("%s got no verdict", pl.pkg.Module)
		}
	}
}

// The cache file has to survive a round trip, because an entry that parses
// back to something else is a wrong answer rather than a slow one.
func TestSourceCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{"a.gno": "aa", "gnomod.toml": "bb"}
	c := openSourceCache(dir, "test-1")
	c.learn("gno.land/r/moul/home", "storage:1,deposit:2", files)

	again := openSourceCache(dir, "test-1")
	got, ok := again.lookup("gno.land/r/moul/home", "storage:1,deposit:2")
	if !ok {
		t.Fatal("the entry did not survive the round trip")
	}
	if len(got) != len(files) || got["a.gno"] != "aa" || got["gnomod.toml"] != "bb" {
		t.Fatalf("read back %v, want %v", got, files)
	}
	if _, ok := again.lookup("gno.land/r/moul/home", "storage:9,deposit:9"); ok {
		t.Fatal("a different storage token was a hit, which is the one thing it must never be")
	}
	// A later line wins, so an entry can be replaced in an append-only file.
	c.learn("gno.land/r/moul/home", "storage:3,deposit:4", map[string]string{"a.gno": "cc"})
	third := openSourceCache(dir, "test-1")
	if got, ok := third.lookup("gno.land/r/moul/home", "storage:3,deposit:4"); !ok || got["a.gno"] != "cc" {
		t.Fatalf("the replacement did not win: %v %v", got, ok)
	}
}
