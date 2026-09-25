package gnopm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeChain is a chain that answers abci_query and nothing else.
//
// Tests point at it with -rpc and -chainid, which skip discovery entirely, so
// nothing here touches the network or needs a gno node. What it has to get
// right is the shape of the two answers that matter: a package that exists,
// and the specific error a chain returns for one that does not, since telling
// those apart is the whole point of the code under test.
type fakeChain struct {
	srv *httptest.Server
	// live and parked are module paths. Anything else is absent.
	live, parked map[string]bool
	// private is the subset of live whose published gnomod.toml declares
	// private = true, which is what CheckPrivate compares the tree against.
	private map[string]bool
	// noInert makes vm/qinertpaths answer the way a chain without the
	// submission policy does: an error, not an empty list.
	noInert bool
	// down makes every query fail at the transport, the case that must never
	// be mistaken for "absent".
	down bool
	// noAccount answers auth/accounts with an empty object, which is what a
	// chain says about an address that has never received funds.
	noAccount bool

	// mu guards calls, and live/parked against the concurrent reads Warm
	// makes. Serial probing needed none of this; a batch does.
	mu sync.Mutex
	// calls counts queries, so a test can assert the caching.
	calls int
}

// count reports the queries served so far.
func (f *fakeChain) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newFakeChain(t *testing.T) *fakeChain {
	t.Helper()
	// Every chain-reading test gets a cache directory of its own. Without this
	// they would share the user's real ~/.gnopm, so a test that says v0 is live
	// would make the next test's "v0 is absent" pass or fail depending on what
	// ran before it, and `go test` would write to a developer's home.
	t.Setenv(cacheEnv, t.TempDir())
	f := &fakeChain{live: map[string]bool{}, parked: map[string]bool{}, private: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls++
		f.mu.Unlock()
		if f.down {
			http.Error(w, "gateway is having a day", http.StatusBadGateway)
			return
		}
		var req struct {
			Params struct {
				Path string `json:"path"`
				Data []byte `json:"data"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch {
		case strings.HasPrefix(req.Params.Path, "auth/accounts/"):
			addr := strings.TrimPrefix(req.Params.Path, "auth/accounts/")
			if f.noAccount {
				writeABCIData(w, `{}`)
				return
			}
			writeABCIData(w, fmt.Sprintf(
				`{"BaseAccount":{"address":%q,"coins":"1000000ugnot","public_key":null,"account_number":"7","sequence":"42"}}`, addr))
			return
		}
		switch req.Params.Path {
		case "vm/qinertpaths":
			if f.noInert {
				writeABCIError(w, "/std.UnknownRequest", "unknown query path")
				return
			}
			var paths []string
			for p := range f.parked {
				paths = append(paths, p)
			}
			writeABCIData(w, strings.Join(paths, "\n"))
		case "vm/qfile":
			p := string(req.Params.Data)
			// `<path>/gnomod.toml` is the one body read rather than counted:
			// a published package's flags are only knowable from the file the
			// chain stored, not from the fact that the path resolves.
			if base, isFile := strings.CutSuffix(p, "/gnomod.toml"); isFile {
				f.mu.Lock()
				isLive, isPrivate := f.live[base], f.private[base]
				f.mu.Unlock()
				if isLive {
					body := "module = \"" + base + "\"\ngno = \"0.9\"\n"
					if isPrivate {
						body += "private = true\n"
					}
					writeABCIData(w, body)
					return
				}
				writeABCIError(w, "/vm.InvalidPkgPathError", "invalid package path - package not found: "+p)
				return
			}
			f.mu.Lock()
			isLive := f.live[p]
			f.mu.Unlock()
			if isLive {
				writeABCIData(w, "file.gno\ngnomod.toml")
				return
			}
			writeABCIError(w, "/vm.InvalidPkgPathError", "invalid package path - package not found: "+p)
		default:
			writeABCIError(w, "/std.UnknownRequest", "unknown query path")
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// probeEnv is the Env a probe test runs under: output discarded, and a cache
// directory of its own, so no test can see another's answers or a user's.
func probeEnv(t *testing.T) *Env {
	t.Helper()
	return &Env{Out: io.Discard, Errw: io.Discard, CacheDir: t.TempDir()}
}

func writeABCIData(w http.ResponseWriter, data string) {
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"response":{"ResponseBase":{"Data":%q}}}}`,
		base64.StdEncoding.EncodeToString([]byte(data)))
}

func writeABCIError(w http.ResponseWriter, typ, log string) {
	fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"response":{"ResponseBase":{"Error":{"@type":%q},"Log":%q}}}}`,
		typ, "msg:\n    --- "+log)
}

// TestProbeSeparatesAnAnswerFromAFailure is the load-bearing distinction.
//
// Before ABCIError existed, ABCIQuery returned a plain error for both "the
// node says there is no such package" and "there is no node", and stateOf read
// either as StateAbsent. Every guard in this file is built on "absent means it
// was published to nobody", so an unreachable chain silently opened all of
// them. No existing test could see it: they all ran against a chain that
// answered.
func TestProbeSeparatesAnAnswerFromAFailure(t *testing.T) {
	f := newFakeChain(t)
	f.live["gno.land/p/moul/md/v0"] = true

	p, err := NewProbe(probeEnv(t), "gno.land/p/moul/md/v0", f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		module string
		want   PackageState
	}{
		{"gno.land/p/moul/md/v0", StateLive},
		{"gno.land/p/moul/md/v1", StateAbsent},
	} {
		got, err := p.State(tc.module)
		if err != nil {
			t.Fatalf("%s: %v", tc.module, err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %s, want %s", tc.module, got, tc.want)
		}
	}

	// Cached: asking again costs no call.
	before := f.count()
	if _, err := p.State("gno.land/p/moul/md/v1"); err != nil {
		t.Fatal(err)
	}
	if f.count() != before {
		t.Fatalf("a second State() made %d more call(s); it should be cached", f.count()-before)
	}

	f.down = true
	if _, err := p.State("gno.land/p/moul/md/v2"); err == nil {
		t.Fatal("an unreachable chain reported a state instead of an error")
	}
}

// TestProbeTreatsParkedAsPublished: a parked package answers "not found" to
// vm/qfile exactly like an absent one, so reading only that would say the
// bytes are still editable when they are already submitted and waiting.
func TestProbeTreatsParkedAsPublished(t *testing.T) {
	f := newFakeChain(t)
	f.parked["gno.land/p/moul/md/v0"] = true
	p, err := NewProbe(probeEnv(t), "gno.land/p/moul/md/v0", f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanPark() {
		t.Fatal("a chain advertising vm/qinertpaths should report CanPark")
	}
	got, err := p.State("gno.land/p/moul/md/v0")
	if err != nil {
		t.Fatal(err)
	}
	if got != StateParked {
		t.Fatalf("got %s, want %s", got, StateParked)
	}
	published, err := p.Published("gno.land/p/moul/md/v0")
	if err != nil {
		t.Fatal(err)
	}
	if !published {
		t.Fatal("parked has to count as published: the bytes cannot be edited and must not be resent")
	}
}

// TestProbeOnAChainThatCannotPark: no inert endpoint is an answer, not a
// failure, and it means absent really is absent.
func TestProbeOnAChainThatCannotPark(t *testing.T) {
	f := newFakeChain(t)
	f.noInert = true
	p, err := NewProbe(probeEnv(t), "gno.land/p/moul/md/v0", f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.CanPark() {
		t.Fatal("a chain without vm/qinertpaths should not report CanPark")
	}
}

// TestProbeDoesNotMistakeADownChainForOneWithoutInert: the same conflation as
// above, one endpoint earlier. A transport failure on vm/qinertpaths used to
// read as "this chain cannot park", which would then let a parked package look
// absent and therefore editable.
func TestProbeDoesNotMistakeADownChainForOneWithoutInert(t *testing.T) {
	f := newFakeChain(t)
	f.down = true
	if _, err := NewProbe(probeEnv(t), "gno.land/p/moul/md/v0", f.srv.URL, "test-1"); err == nil {
		t.Fatal("NewProbe against an unreachable chain should fail, not report a chain that cannot park")
	}
}

// --- bump -if-published ---

// bumpRepo builds a workspace whose single package has already been bumped
// once, so both a published and an unpublished previous version are available
// to test against.
func bumpRepo(t *testing.T) string {
	t.Helper()
	root := newRepo(t)
	addPkg(t, root, "p/moul/md", "gno.land/p/moul/md/v0", "package md\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	commit(t, root, "lock")
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	return root
}

func moduleLine(t *testing.T, root, dir string) string {
	t.Helper()
	mod, _, err := readGnomod(filepath.Join(root, filepath.FromSlash(dir), "gnomod.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return mod
}

func TestBumpIfPublished(t *testing.T) {
	for _, tc := range []struct {
		name       string
		live       bool
		parked     bool
		wantModule string
		wantSaid   string
	}{
		{
			name: "absent leaves the version alone", wantModule: "gno.land/p/moul/md/v0",
			wantSaid: "not bumping",
		},
		{
			name: "live earns a new number", live: true, wantModule: "gno.land/p/moul/md/v1",
			wantSaid: "it cannot be edited",
		},
		{
			name: "parked earns one too", parked: true, wantModule: "gno.land/p/moul/md/v1",
			wantSaid: "it cannot be edited",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := bumpRepo(t)
			f := newFakeChain(t)
			if tc.live {
				f.live["gno.land/p/moul/md/v0"] = true
			}
			if tc.parked {
				f.parked["gno.land/p/moul/md/v0"] = true
			}
			var out bytes.Buffer
			err := Bump(testEnv(root, &out), "md", BumpOptions{
				IfPublished: true, RPC: f.srv.URL, ChainID: "test-1",
			})
			if err != nil {
				t.Fatalf("declining is not an error: %v", err)
			}
			if got := moduleLine(t, root, "p/moul/md"); got != tc.wantModule {
				t.Fatalf("module line is %q, want %q\n%s", got, tc.wantModule, out.String())
			}
			if !strings.Contains(out.String(), tc.wantSaid) {
				t.Fatalf("output does not say %q:\n%s", tc.wantSaid, out.String())
			}
		})
	}
}

// TestBumpIfPublishedStopsOnAnUnreachableChain: the failure mode that makes
// the guard worse than no guard. Silently deciding "absent, so do not bump"
// because the RPC was down would leave the author editing a version that is
// live for everyone else.
func TestBumpIfPublishedStopsOnAnUnreachableChain(t *testing.T) {
	root := bumpRepo(t)
	f := newFakeChain(t)
	f.down = true
	var out bytes.Buffer
	err := Bump(testEnv(root, &out), "md", BumpOptions{
		IfPublished: true, RPC: f.srv.URL, ChainID: "test-1",
	})
	if err == nil {
		t.Fatalf("an unreachable chain has to be an error, not a decision:\n%s", out.String())
	}
	if got := moduleLine(t, root, "p/moul/md"); got != "gno.land/p/moul/md/v0" {
		t.Fatalf("module line moved to %q despite the failure", got)
	}
}

// TestBumpStaysOfflineByDefault: no -if-published, no chain read. A chain that
// is down must not be able to stop an ordinary bump, or gnopm stops working on
// a plane and in CI.
func TestBumpStaysOfflineByDefault(t *testing.T) {
	root := bumpRepo(t)
	f := newFakeChain(t)
	f.down = true
	if err := Bump(testEnv(root, &bytes.Buffer{}), "md", BumpOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.count() != 0 {
		t.Fatalf("a plain bump made %d chain call(s); it must make none", f.count())
	}
	if got := moduleLine(t, root, "p/moul/md"); got != "gno.land/p/moul/md/v1" {
		t.Fatalf("module line is %q, want v1", got)
	}
}

// --- unbump ---

// bumpedRepo builds the situation unbump exists for: a package bumped to v1
// with v0 pinned behind it, neither published.
func bumpedRepo(t *testing.T) string {
	t.Helper()
	root := bumpRepo(t)
	if err := Bump(testEnv(root, &bytes.Buffer{}), "md", BumpOptions{}); err != nil {
		t.Fatal(err)
	}
	commit(t, root, "bump to v1")
	return root
}

func TestUnbumpFoldsAnUnpublishedVersion(t *testing.T) {
	root := bumpedRepo(t)
	f := newFakeChain(t)
	var out bytes.Buffer
	if err := Unbump(testEnv(root, &out), "md", UnbumpOptions{RPC: f.srv.URL, ChainID: "test-1"}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := moduleLine(t, root, "p/moul/md"); got != "gno.land/p/moul/md/v0" {
		t.Fatalf("module line is %q, want v0", got)
	}
	lock, err := readLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Modules) != 1 {
		t.Fatalf("lock has %d entries, want 1: %s", len(lock.Modules), lock.String())
	}
	en := lock.Modules[0]
	if en.Module != "gno.land/p/moul/md/v0" || !en.Source.InTree() {
		t.Fatalf("lock entry is %+v, want v0 in the tree", en)
	}
}

// TestUnbumpLeavesNoStaleDirEntry pins the trap that made unbump a command
// instead of "rewrite the module line and run sync".
//
// buildLock carries over every entry whose module is not in the working tree,
// so relocking after a hand-edited module line keeps the abandoned version as
// a { dir } entry pointing at the directory that no longer declares it. Two
// modules then claim one directory, nothing errors, and whatever imports the
// abandoned version silently resolves to the replacement.
func TestUnbumpLeavesNoStaleDirEntry(t *testing.T) {
	root := bumpedRepo(t)
	f := newFakeChain(t)
	if err := Unbump(testEnv(root, &bytes.Buffer{}), "md", UnbumpOptions{RPC: f.srv.URL, ChainID: "test-1"}); err != nil {
		t.Fatal(err)
	}
	lock, err := readLock(root)
	if err != nil {
		t.Fatal(err)
	}
	byDir := map[string]string{}
	for _, en := range lock.Modules {
		if !en.Source.InTree() {
			continue
		}
		if prev, dup := byDir[en.Source.Dir]; dup {
			t.Fatalf("%q and %q both claim %s", prev, en.Module, en.Source.Dir)
		}
		byDir[en.Source.Dir] = en.Module
	}
	// And the whole thing still reproduces.
	if err := Verify(testEnv(root, &bytes.Buffer{})); err != nil {
		t.Fatalf("after unbump the workspace should verify: %v", err)
	}
}

// TestUnbumpRefusesToWithdrawAPublishedVersion: the one thing the chain will
// not allow. Something already resolves that path.
func TestUnbumpRefusesToWithdrawAPublishedVersion(t *testing.T) {
	root := bumpedRepo(t)
	f := newFakeChain(t)
	f.live["gno.land/p/moul/md/v1"] = true
	var out bytes.Buffer
	err := Unbump(testEnv(root, &out), "md", UnbumpOptions{RPC: f.srv.URL, ChainID: "test-1"})
	if err == nil {
		t.Fatalf("unbump withdrew a published version:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "cannot be withdrawn") {
		t.Fatalf("error does not say why: %v", err)
	}
	if got := moduleLine(t, root, "p/moul/md"); got != "gno.land/p/moul/md/v1" {
		t.Fatalf("module line moved to %q despite the refusal", got)
	}
}

// TestUnbumpIsANoOpWhenTheNumberIsAlreadyRight: v0 published and the tree at
// v1 is a correct bump, so there is nothing to do and nothing to complain
// about. It cannot be an error: tidy names unbump as the fix, and a fix that
// exits non-zero when it was not needed cannot be run from a script.
//
// The target is read off the chain rather than off the lock, which is what
// makes this structural: v1 is above every published version, so there is no
// published content below it to redefine and no guard needed to say so.
func TestUnbumpIsANoOpWhenTheNumberIsAlreadyRight(t *testing.T) {
	root := bumpedRepo(t)
	f := newFakeChain(t)
	f.live["gno.land/p/moul/md/v0"] = true
	var out bytes.Buffer
	if err := Unbump(testEnv(root, &out), "md", UnbumpOptions{RPC: f.srv.URL, ChainID: "test-1"}); err != nil {
		t.Fatalf("a correct version is not an error: %v", err)
	}
	if got := moduleLine(t, root, "p/moul/md"); got != "gno.land/p/moul/md/v1" {
		t.Fatalf("module line moved to %q; v1 is already right", got)
	}
	if !strings.Contains(out.String(), "already the lowest free version") {
		t.Fatalf("output does not explain the no-op:\n%s", out.String())
	}
}

// TestUnbumpClosesAHoleTheLockCannotSee is the case that forced the target to
// come off the chain instead of out of the lock.
//
// On a real stacked branch the intermediate version is often not in the lock
// at all: bump cannot pin a version that lives on no commit the default branch
// has, so the number is skipped rather than recorded, and the lock jumps v0 to
// v2 with nothing in between. previousVersion then answers v0, which is
// published, and a lock-based unbump would either refuse or fold onto live
// content. Asking the chain for the lowest free number answers v1, which is
// both correct and invisible locally.
func TestUnbumpClosesAHoleTheLockCannotSee(t *testing.T) {
	root := bumpRepo(t)
	// v0 shipped; the branch skipped v1 entirely and went to v2.
	if err := Bump(testEnv(root, &bytes.Buffer{}), "md", BumpOptions{To: 2}); err != nil {
		t.Fatal(err)
	}
	commit(t, root, "bump to v2")
	lock, err := readLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, held, _ := findModule(lock, "gno.land/p/moul/md/v1"); held {
		t.Fatal("this test needs v1 absent from the lock")
	}

	f := newFakeChain(t)
	f.live["gno.land/p/moul/md/v0"] = true
	var out bytes.Buffer
	if err := Unbump(testEnv(root, &out), "md", UnbumpOptions{RPC: f.srv.URL, ChainID: "test-1"}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := moduleLine(t, root, "p/moul/md"); got != "gno.land/p/moul/md/v1" {
		t.Fatalf("module line is %q, want v1: the lowest number the chain has never seen", got)
	}
	// v0 keeps its pin: it is live, and something may still import it.
	lock, err = readLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, held, _ := findModule(lock, "gno.land/p/moul/md/v0"); !held {
		t.Fatalf("unbump dropped the pin for a published version:\n%s", lock.String())
	}
	if _, held, _ := findModule(lock, "gno.land/p/moul/md/v2"); held {
		t.Fatalf("v2 survived in the lock:\n%s", lock.String())
	}
}

// TestUnbumpRefusesWhileSomethingImportsIt: folding away a version something
// imports would leave that import resolving to nothing.
func TestUnbumpRefusesWhileSomethingImportsIt(t *testing.T) {
	root := bumpedRepo(t)
	addPkg(t, root, "p/moul/user", "gno.land/p/moul/user/v0",
		"package user\n\nimport \"gno.land/p/moul/md/v1\"\n\nfunc U() { _ = md.X }\n")
	commit(t, root, "add an importer")
	mustRun(t, root, "sync")
	f := newFakeChain(t)
	err := Unbump(testEnv(root, &bytes.Buffer{}), "md", UnbumpOptions{RPC: f.srv.URL, ChainID: "test-1"})
	if err == nil {
		t.Fatal("unbump folded away a version that is still imported")
	}
	if !strings.Contains(err.Error(), "still imports") {
		t.Fatalf("error does not name the importer problem: %v", err)
	}
}

// TestUnbumpStopsAtV0: there is no number below it, and working that out
// needs no chain.
func TestUnbumpStopsAtV0(t *testing.T) {
	root := bumpRepo(t)
	f := newFakeChain(t)
	err := Unbump(testEnv(root, &bytes.Buffer{}), "md", UnbumpOptions{RPC: f.srv.URL, ChainID: "test-1"})
	if err == nil {
		t.Fatal("unbump invented a version below v0")
	}
	if !strings.Contains(err.Error(), "already at v0") {
		t.Fatalf("error does not explain: %v", err)
	}
	if f.count() != 0 {
		t.Fatalf("unbump read the chain %d time(s) before working out there was nothing to do", f.count())
	}
}

// TestUnbumpDropsEveryNumberItSkipsPast: folding v3 down to v1 discards v2 as
// well, and leaving its pin behind would put a version above the one the tree
// declares.
func TestUnbumpDropsEveryNumberItSkipsPast(t *testing.T) {
	root := bumpRepo(t)
	for i := 0; i < 3; i++ {
		write(t, filepath.Join(root, "p/moul/md/md.gno"), fmt.Sprintf("package md // %d\n", i))
		commit(t, root, "edit")
		if err := Bump(testEnv(root, &bytes.Buffer{}), "md", BumpOptions{}); err != nil {
			t.Fatal(err)
		}
		commit(t, root, "bump")
	}
	if got := moduleLine(t, root, "p/moul/md"); got != "gno.land/p/moul/md/v3" {
		t.Fatalf("setup is at %q, want v3", got)
	}
	f := newFakeChain(t)
	f.live["gno.land/p/moul/md/v0"] = true
	if err := Unbump(testEnv(root, &bytes.Buffer{}), "md", UnbumpOptions{RPC: f.srv.URL, ChainID: "test-1"}); err != nil {
		t.Fatal(err)
	}
	if got := moduleLine(t, root, "p/moul/md"); got != "gno.land/p/moul/md/v1" {
		t.Fatalf("module line is %q, want v1", got)
	}
	lock, err := readLock(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"gno.land/p/moul/md/v2", "gno.land/p/moul/md/v3"} {
		if _, held, _ := findModule(lock, m); held {
			t.Fatalf("%s survived above the tree version:\n%s", m, lock.String())
		}
	}
	// The upstream half of verify is about this test's own setup, not about
	// unbump: three bump commits landed after origin/main was set.
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	if err := Verify(testEnv(root, &bytes.Buffer{})); err != nil {
		t.Fatalf("after unbump the workspace should verify: %v", err)
	}
}

// TestUnbumpForceSkipsTheChain: for a workspace whose chain is unreachable.
func TestUnbumpForceSkipsTheChain(t *testing.T) {
	root := bumpedRepo(t)
	f := newFakeChain(t)
	f.down = true
	if err := Unbump(testEnv(root, &bytes.Buffer{}), "md", UnbumpOptions{Force: true, RPC: f.srv.URL, ChainID: "test-1"}); err != nil {
		t.Fatal(err)
	}
	if f.count() != 0 {
		t.Fatalf("-force still made %d chain call(s)", f.count())
	}
	if got := moduleLine(t, root, "p/moul/md"); got != "gno.land/p/moul/md/v0" {
		t.Fatalf("module line is %q, want v0", got)
	}
}

// TestUnbumpFollowsTheLockNotTheArithmetic: `bump -to 5` leaves holes in the
// sequence on purpose, so unbump from v5 has to land on the version the lock
// actually holds and not on an invented v4.
func TestUnbumpFollowsTheLockNotTheArithmetic(t *testing.T) {
	root := bumpRepo(t)
	if err := Bump(testEnv(root, &bytes.Buffer{}), "md", BumpOptions{To: 5}); err != nil {
		t.Fatal(err)
	}
	commit(t, root, "bump to v5")
	f := newFakeChain(t)
	if err := Unbump(testEnv(root, &bytes.Buffer{}), "md", UnbumpOptions{RPC: f.srv.URL, ChainID: "test-1"}); err != nil {
		t.Fatal(err)
	}
	if got := moduleLine(t, root, "p/moul/md"); got != "gno.land/p/moul/md/v0" {
		t.Fatalf("module line is %q, want v0", got)
	}
}

// --- tidy's chain pass ---

// TestTidyNamesTheVersionSpentOnNothing is the case the whole change exists
// for: two versions in a row, neither ever published, so the bump between them
// bought a number and nothing else. No local check can see it, because the
// commit holding v0 is perfectly reachable from the default branch.
func TestTidyNamesTheVersionSpentOnNothing(t *testing.T) {
	root := bumpedRepo(t)
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	f := newFakeChain(t)
	var out bytes.Buffer
	if err := Tidy(testEnv(root, &out), TidyOptions{RPC: f.srv.URL, ChainID: "test-1"}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"spent on nothing", "gnopm unbump p/moul/md", "gno.land/p/moul/md/v1", "v0 is free"} {
		if !strings.Contains(got, want) {
			t.Fatalf("tidy did not say %q:\n%s", want, got)
		}
	}
}

// TestTidySaysNothingWhenThePreviousVersionShipped: v0 live means the bump to
// v1 was exactly right, and tidy must not nag about it.
func TestTidySaysNothingWhenThePreviousVersionShipped(t *testing.T) {
	root := bumpedRepo(t)
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	f := newFakeChain(t)
	f.live["gno.land/p/moul/md/v0"] = true
	var out bytes.Buffer
	if err := Tidy(testEnv(root, &out), TidyOptions{RPC: f.srv.URL, ChainID: "test-1"}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "spent on nothing") {
		t.Fatalf("tidy called a correct bump wasteful:\n%s", out.String())
	}
}

// TestTidyDropsBeforeItVerifies pins the pass order.
//
// verify rejects a pin to a commit that is not on the default branch, and
// dropping exactly those pins is what the drop pass does. Proving first made
// tidy fail with the error its own next pass exists to remove, which is a
// command that can never succeed on the workspace it was written for.
func TestTidyDropsBeforeItVerifies(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/moul/md", "gno.land/p/moul/md/v0", "package md\n")
	commit(t, root, "seed")
	mustRun(t, root, "sync")
	commit(t, root, "lock")
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")

	// Edit then bump, on a branch, so v0's pin lives only here.
	gitCmd(t, root, "checkout", "-q", "-b", "feature")
	write(t, filepath.Join(root, "p/moul/md/md.gno"), "package md // edited\n")
	commit(t, root, "edit")
	if err := Bump(testEnv(root, &bytes.Buffer{}), "md", BumpOptions{}); err != nil {
		t.Fatal(err)
	}
	commit(t, root, "bump")

	f := newFakeChain(t)
	var out bytes.Buffer
	if err := Tidy(testEnv(root, &out), TidyOptions{RPC: f.srv.URL, ChainID: "test-1"}); err != nil {
		t.Fatalf("tidy failed on the workspace it exists to fix: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "drop gno.land/p/moul/md/v0") {
		t.Fatalf("tidy did not drop the stranded pin:\n%s", out.String())
	}
}

// TestTidyOfflineSkipsTheChain keeps tidy usable with no network.
func TestTidyOfflineSkipsTheChain(t *testing.T) {
	root := bumpedRepo(t)
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	f := newFakeChain(t)
	var out bytes.Buffer
	if err := Tidy(testEnv(root, &out), TidyOptions{Offline: true, RPC: f.srv.URL, ChainID: "test-1"}); err != nil {
		t.Fatal(err)
	}
	if f.count() != 0 {
		t.Fatalf("-offline made %d chain call(s)", f.count())
	}
	if !strings.Contains(out.String(), "skipped (-offline)") {
		t.Fatalf("tidy did not say it skipped the chain:\n%s", out.String())
	}
}

// TestTidyDryRunWritesNothing.
func TestTidyDryRunWritesNothing(t *testing.T) {
	root := bumpedRepo(t)
	before, err := os.ReadFile(filepath.Join(root, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeChain(t)
	if err := Tidy(testEnv(root, &bytes.Buffer{}), TidyOptions{DryRun: true, RPC: f.srv.URL, ChainID: "test-1"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, lockFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("-n rewrote %s", lockFile)
	}
}

// TestProbeWarmAsksOncePerPathAndCachesTheAnswers.
//
// Reading a chain about N packages was N blocking round trips, one after the
// other, which on a real workspace is a minute of a terminal that looks hung.
// Nothing about the answers interacts, so the batch parallelizes exactly; what
// it must not do is ask twice, or leave State to ask again afterwards.
func TestProbeWarmAsksOncePerPathAndCachesTheAnswers(t *testing.T) {
	f := newFakeChain(t)
	modules := []string{}
	for i := 0; i < 12; i++ {
		m := fmt.Sprintf("gno.land/p/moul/pkg%02d/v0", i)
		modules = append(modules, m)
		if i%2 == 0 {
			f.live[m] = true
		}
	}
	p, err := NewProbe(probeEnv(t), modules[0], f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	afterInert := f.count() // the one vm/qinertpaths read NewProbe makes

	var mu sync.Mutex
	var seen []string
	totals := map[int]bool{}
	// The same path twice, and a duplicate of one already asked for: neither
	// should cost a query.
	if err := p.Warm(append(modules, modules[0], modules[3]), func(done, total int, m string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, m)
		totals[total] = true
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.count() - afterInert; got != len(modules) {
		t.Fatalf("Warm made %d queries for %d distinct paths", got, len(modules))
	}
	if len(seen) != len(modules) {
		t.Fatalf("step was called %d time(s), want %d", len(seen), len(modules))
	}
	if len(totals) != 1 || !totals[len(modules)] {
		t.Fatalf("step reported totals %v, want only %d", totals, len(modules))
	}

	before := f.count()
	for i, m := range modules {
		s, err := p.State(m)
		if err != nil {
			t.Fatal(err)
		}
		want := StateAbsent
		if i%2 == 0 {
			want = StateLive
		}
		if s != want {
			t.Fatalf("State(%s) = %s, want %s", m, s, want)
		}
	}
	if f.count() != before {
		t.Fatalf("State went back to the chain %d time(s) after Warm", f.count()-before)
	}
	// A second Warm over the same set is free.
	if err := p.Warm(modules, nil); err != nil {
		t.Fatal(err)
	}
	if f.count() != before {
		t.Fatalf("a second Warm made %d more query/queries", f.count()-before)
	}
}

// Claim 7, in the batch: a chain that cannot be reached must not come back as
// a set of absent packages. Serial probing got this right because a single
// error stopped it; a batch has to carry the first transport failure out
// rather than let the rest of the workers fill the cache around it.
func TestProbeWarmReportsATransportFailureRatherThanAbsence(t *testing.T) {
	f := newFakeChain(t)
	p, err := NewProbe(probeEnv(t), "gno.land/p/moul/md/v0", f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	f.down = true
	modules := []string{}
	for i := 0; i < 12; i++ {
		modules = append(modules, fmt.Sprintf("gno.land/p/moul/pkg%02d/v0", i))
	}
	err = p.Warm(modules, nil)
	if err == nil {
		t.Fatal("Warm read an unreachable chain as a workspace of absent packages")
	}
	if answered(err) {
		t.Fatalf("a transport failure came back as a chain answer: %v", err)
	}
	// Nothing may be cached from a failed batch, or the next State returns an
	// answer the chain never gave.
	p.mu.Lock()
	n := len(p.cache)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("a failed batch cached %d state(s)", n)
	}
}
