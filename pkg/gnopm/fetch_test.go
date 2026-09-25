package gnopm

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chainWorkspace builds a repo importing one package that lives only on a
// chain, plus the chain that has it.
func chainWorkspace(t *testing.T) (root string, f *fakeChain, cache string) {
	t.Helper()
	root = newRepo(t)
	addPkg(t, root, "p/me/app", "gno.land/p/me/app/v0",
		"package app\n\nimport \"gno.land/p/nt/tinyavl/v0\"\n\nfunc A() string { return tinyavl.Get(\"hi\") }\n")
	commit(t, root, "initial")
	f = newFakeChain(t)
	f.files = map[string]string{
		"gno.land/p/nt/tinyavl/v0/gnomod.toml":      "module = \"gno.land/p/nt/tinyavl/v0\"\ngno = \"0.9\"\n",
		"gno.land/p/nt/tinyavl/v0/tinyavl.gno":      "package tinyavl\n\nfunc Get(k string) string { return k }\n",
		"gno.land/p/nt/tinyavl/v0/tinyavl_test.gno": "package tinyavl\n\nimport \"testing\"\n\nfunc TestGet(t *testing.T) { _ = Get(\"x\") }\n",
	}
	return root, f, t.TempDir()
}

func runGet(t *testing.T, root, cache, rpc string, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv(cacheEnv, cache)
	var out, errb bytes.Buffer
	full := append([]string{"get", "-C", root, "-rpc", rpc, "-chainid", "test"}, args...)
	err := Run(full, &out, &errb)
	return out.String(), errb.String(), err
}

// TestGetFetchesAPackageThatExistsOnlyOnAChain is the whole of stage 1 of #53,
// end to end: the fetch, the cache, the lock entry, the assembly, and the fact
// that the import then resolves.
//
// The assertion that matters most is the last one. Recording a hash and writing
// a lock entry is easy to get right while putting the wrong bytes on disk, and
// nothing downstream would notice until a build failed for an unrelated-looking
// reason.
func TestGetFetchesAPackageThatExistsOnlyOnAChain(t *testing.T) {
	root, f, cache := chainWorkspace(t)

	_, errb, err := runGet(t, root, cache, f.srv.URL, "gno.land/p/nt/tinyavl/v0")
	if err != nil {
		t.Fatalf("get: %v\n%s", err, errb)
	}

	// The lock records the chain and the hash, and nothing about this machine.
	en, ok := mustByModule(t, root)["gno.land/p/nt/tinyavl/v0"]
	if !ok {
		t.Fatal("the fetched package is not in the lock")
	}
	if got := en.Source.Variant(); got != "chain" {
		t.Fatalf("source variant is %q, want chain: %+v", got, en.Source)
	}
	if en.Source.Chain != "test" {
		t.Errorf("chain is %q, want test", en.Source.Chain)
	}
	if en.Source.Dir != "" || en.Source.Commit != "" {
		t.Errorf("a chain entry names a local place: %+v", en.Source)
	}
	if !strings.HasPrefix(en.Hash, hashPrefix) {
		t.Errorf("hash is %q", en.Hash)
	}

	// The files are in the shared cache, keyed by chain id so the same path on
	// two chains cannot collide.
	cached := filepath.Join(cache, downloadSubdir, "test", "gno.land", "p", "nt", "tinyavl", "v0")
	if _, err := os.Stat(filepath.Join(cached, "tinyavl.gno")); err != nil {
		t.Errorf("not in the download cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cached, metaFile)); err != nil {
		t.Errorf("no provenance written: %v", err)
	}

	// And the bytes are the chain's bytes, in the place the toolchain reads.
	got := read(t, filepath.Join(root, assemblyDir, "gno.land", "p", "nt", "tinyavl", "v0", "tinyavl.gno"))
	if want := "package tinyavl\n\nfunc Get(k string) string { return k }\n"; got != want {
		t.Errorf("the materialized source is not what the chain served:\n got: %q\nwant: %q", got, want)
	}

	// Test files are part of what was deployed, so they come too. Dropping
	// them would make a downloaded package hash differently from the same
	// package vendored, for no reason a user could see.
	if _, err := os.Stat(filepath.Join(root, assemblyDir, "gno.land/p/nt/tinyavl/v0/tinyavl_test.gno")); err != nil {
		t.Errorf("test files were dropped: %v", err)
	}

	mustRun(t, root, "verify")
	if s := mustRun(t, root, "status"); !strings.Contains(s, "up to date") {
		t.Fatalf("get left the workspace stale:\n%s", s)
	}
	if s := mustRun(t, root, "ls"); !strings.Contains(s, "chain") || !strings.Contains(s, "test") {
		t.Errorf("ls does not say where it came from:\n%s", s)
	}
}

// TestSyncFromCacheNeedsNoChain is principle 8: a workspace whose dependencies
// are already downloaded completes with no network at all.
//
// Proved by closing the server, not by trusting a log line. If sync reaches for
// the chain the connection is refused and the test fails, which is the only
// version of this assertion worth having.
func TestSyncFromCacheNeedsNoChain(t *testing.T) {
	root, f, cache := chainWorkspace(t)
	if _, errb, err := runGet(t, root, cache, f.srv.URL, "gno.land/p/nt/tinyavl/v0"); err != nil {
		t.Fatalf("get: %v\n%s", err, errb)
	}
	mustRun(t, root, "clean")
	f.srv.Close()

	t.Setenv(cacheEnv, cache)
	var out, errb bytes.Buffer
	if err := Run([]string{"sync", "-C", root}, &out, &errb); err != nil {
		t.Fatalf("sync went to the chain when the cache had the answer: %v\n%s", err, errb.String())
	}
	if _, err := os.Stat(filepath.Join(root, assemblyDir, "gno.land/p/nt/tinyavl/v0/tinyavl.gno")); err != nil {
		t.Fatalf("sync did not rebuild from the cache: %v", err)
	}
	mustRun(t, root, "verify")
}

// TestVerifyProvesAChainDependency: a chain entry has no commit, so git cannot
// prove it, and verify used to try anyway and report
// `commit "" is not in this repository`.
//
// What proves it is the same h1 hash over the materialized copy. The chain
// cannot redefine a published path, so bytes that hash right are the bytes that
// were locked.
func TestVerifyProvesAChainDependency(t *testing.T) {
	root, f, cache := chainWorkspace(t)
	if _, errb, err := runGet(t, root, cache, f.srv.URL, "gno.land/p/nt/tinyavl/v0"); err != nil {
		t.Fatalf("get: %v\n%s", err, errb)
	}
	mustRun(t, root, "verify")

	// Tamper with what was materialized. verify has to notice.
	p := filepath.Join(root, assemblyDir, "gno.land/p/nt/tinyavl/v0/tinyavl.gno")
	write(t, p, read(t, p)+"\n// tampered\n")

	var out, errb bytes.Buffer
	if err := Run([]string{"verify", "-C", root}, &out, &errb); err == nil {
		t.Fatal("verify accepted a chain dependency whose bytes had changed")
	}
	if got := errb.String(); !strings.Contains(got, "hashes to") {
		t.Errorf("verify did not say what was wrong:\n%s", got)
	}
}

// TestGetRefusesWhatTheWorkspaceAlreadyHas. Two sources for one module path is
// the single way to make the toolchain's resolution ambiguous, and it would be
// silent, so both shapes are refused rather than merged.
func TestGetRefusesWhatTheWorkspaceAlreadyHas(t *testing.T) {
	root, f, cache := chainWorkspace(t)

	_, _, err := runGet(t, root, cache, f.srv.URL, "gno.land/p/me/app/v0")
	if err == nil {
		t.Fatal("get fetched a package the working tree already declares")
	}
	if !strings.Contains(err.Error(), "already in this workspace") {
		t.Errorf("unhelpful error: %v", err)
	}

	// The same for a version pinned to this repository's own history: fetching
	// it from a chain would swap something reproducible for something that is
	// not.
	mustRun(t, root, "sync")
	mustRun(t, root, "bump", "p/me/app")
	if _, _, err := runGet(t, root, cache, f.srv.URL, "gno.land/p/me/app/v0"); err == nil {
		t.Fatal("get overwrote a version pinned to history")
	}
}

// TestGetRefusesAChainThatDisagreesWithTheLock. A lock naming one chain id
// cannot be satisfied by another network: its hashes describe packages that one
// has never seen, so downloading and then verifying against them would be
// checking the wrong expectation.
func TestChainIDMismatchIsRefused(t *testing.T) {
	root, f, cache := chainWorkspace(t)
	if _, errb, err := runGet(t, root, cache, f.srv.URL, "gno.land/p/nt/tinyavl/v0"); err != nil {
		t.Fatalf("get: %v\n%s", err, errb)
	}
	// Rewrite the lock to claim another network, then drop the cache so
	// something actually has to be fetched.
	lock := read(t, filepath.Join(root, lockFile))
	write(t, filepath.Join(root, lockFile), strings.ReplaceAll(lock, `chain = "test"`, `chain = "other-1"`))
	mustRun(t, root, "clean")

	t.Setenv(cacheEnv, cache)
	var out, errb bytes.Buffer
	err := Run([]string{"sync", "-C", root, "-rpc", f.srv.URL, "-chainid", "test"}, &out, &errb)
	if err == nil {
		t.Fatal("a lock written against another network was satisfied anyway")
	}
	if !strings.Contains(err.Error(), "cannot be satisfied by another") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// TestVendorMakesTheWorkspaceSelfContained is stage 2 of #53.
//
// The assertion that carries it is the last one: the chain is closed AND the
// download cache is emptied, and the workspace still syncs and verifies. Either
// one alone would pass against a gnopm that had quietly fallen back to the
// other, which is exactly the bug vendoring exists to make impossible.
func TestVendorMakesTheWorkspaceSelfContained(t *testing.T) {
	root, f, cache := chainWorkspace(t)
	if _, errb, err := runGet(t, root, cache, f.srv.URL, "gno.land/p/nt/tinyavl/v0"); err != nil {
		t.Fatalf("get: %v\n%s", err, errb)
	}

	t.Setenv(cacheEnv, cache)
	var out, errb bytes.Buffer
	if err := Run([]string{"vendor", "-C", root}, &out, &errb); err != nil {
		t.Fatalf("vendor: %v\n%s", err, errb.String())
	}

	// The bytes are committed source now, at a path that says what they are.
	got := read(t, filepath.Join(root, vendorDir, "gno.land/p/nt/tinyavl/v0/tinyavl.gno"))
	if want := "package tinyavl\n\nfunc Get(k string) string { return k }\n"; got != want {
		t.Errorf("vendored source is wrong:\n got: %q\nwant: %q", got, want)
	}

	// And the assembly copy is gone. Two directories under the workspace root
	// declaring one module path is the ambiguity every other guard here exists
	// to prevent, so vendoring must not create one.
	if _, err := os.Stat(filepath.Join(root, assemblyDir, "gno.land/p/nt/tinyavl/v0")); !os.IsNotExist(err) {
		t.Errorf("the assembly still holds a copy of a vendored package: %v", err)
	}

	// The lock is untouched: still { chain }, still carrying the provenance
	// and the hash. Vendoring is not a different kind of dependency.
	en := mustByModule(t, root)["gno.land/p/nt/tinyavl/v0"]
	if en.Source.Variant() != "chain" || en.Source.Chain != "test" {
		t.Errorf("vendoring rewrote the lock: %+v", en.Source)
	}

	// Now take away everything else there is.
	f.srv.Close()
	if err := os.RemoveAll(filepath.Join(cache, downloadSubdir)); err != nil {
		t.Fatal(err)
	}
	mustRun(t, root, "sync")
	mustRun(t, root, "verify")
	if s := mustRun(t, root, "status"); !strings.Contains(s, "up to date") {
		t.Fatalf("a vendored workspace is not settled:\n%s", s)
	}
}

// TestVendoredSourceIsStillProved: vendor/ is committed, so it is exactly the
// place somebody can edit by hand or mis-merge. The lock's hash is what keeps
// it honest, and it has to apply to the vendored copy and not only to a
// downloaded one.
func TestVendoredSourceIsStillProved(t *testing.T) {
	root, f, cache := chainWorkspace(t)
	if _, errb, err := runGet(t, root, cache, f.srv.URL, "gno.land/p/nt/tinyavl/v0"); err != nil {
		t.Fatalf("get: %v\n%s", err, errb)
	}
	t.Setenv(cacheEnv, cache)
	var out, errb bytes.Buffer
	if err := Run([]string{"vendor", "-C", root}, &out, &errb); err != nil {
		t.Fatalf("vendor: %v\n%s", err, errb.String())
	}

	p := filepath.Join(root, vendorDir, "gno.land/p/nt/tinyavl/v0/tinyavl.gno")
	write(t, p, read(t, p)+"\n// edited in vendor/\n")

	var vout, verr bytes.Buffer
	if err := Run([]string{"verify", "-C", root}, &vout, &verr); err == nil {
		t.Fatal("verify accepted a hand-edited vendored dependency")
	}
	if got := verr.String(); !strings.Contains(got, "hashes to") {
		t.Errorf("verify did not say what was wrong:\n%s", got)
	}
}

// TestVendorSaysSoWhenThereIsNothingToDo. A command that prints nothing on a
// workspace it cannot help is a command people run twice and then distrust.
func TestVendorWithNoChainDependencies(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	var out, errb bytes.Buffer
	if err := Run([]string{"vendor", "-C", root}, &out, &errb); err != nil {
		t.Fatalf("vendor: %v", err)
	}
	if !strings.Contains(errb.String(), "nothing to vendor") {
		t.Errorf("vendor was silent about having nothing to do:\n%s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(root, vendorDir)); !os.IsNotExist(err) {
		t.Error("vendor created an empty vendor/ directory")
	}
}

// TestUnvendoringFallsBack is the other direction, and the one the assembly
// stamp made fail.
//
// Vendoring changes what the assembly should hold without changing the lock, so
// a stamp over the lock alone said "already up to date" and install did
// nothing. Deleting vendor/ then left a workspace that resolved nothing and
// whose own status called itself fine.
func TestUnvendoringFallsBack(t *testing.T) {
	root, f, cache := chainWorkspace(t)
	if _, errb, err := runGet(t, root, cache, f.srv.URL, "gno.land/p/nt/tinyavl/v0"); err != nil {
		t.Fatalf("get: %v\n%s", err, errb)
	}
	t.Setenv(cacheEnv, cache)
	var out, errb bytes.Buffer
	if err := Run([]string{"vendor", "-C", root}, &out, &errb); err != nil {
		t.Fatalf("vendor: %v\n%s", err, errb.String())
	}

	// Unvendoring is deleting the directory, which the command's own output
	// tells you. The chain stays closed: the cache must be enough.
	f.srv.Close()
	if err := os.RemoveAll(filepath.Join(root, vendorDir)); err != nil {
		t.Fatal(err)
	}
	mustRun(t, root, "sync")

	if _, err := os.Stat(filepath.Join(root, assemblyDir, "gno.land/p/nt/tinyavl/v0/tinyavl.gno")); err != nil {
		t.Fatalf("sync did not bring the assembly copy back: %v", err)
	}
	mustRun(t, root, "verify")
}
