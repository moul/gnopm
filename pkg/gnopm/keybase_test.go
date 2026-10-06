package gnopm

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// The keybase, for tests: what `gnokey list` answers, decided here rather than
// by whatever is installed on the machine running them.
//
// Before this, `go test ./...` passed or failed on whether the developer had
// gnokey with a key in it: green on CI, which has no keybase, and red on every
// machine that could actually notice a regression (#70). The branch that runs
// on a real machine, where an address resolves, was also the branch CI could
// never reach.

// TestMain cuts the whole package off from the machine's keybase.
//
// The default is the CI shape, gnokey not installed, so no test can read a
// developer's wallet by accident and both environments now see the same thing.
// withKeys opts a single test into the other branch.
func TestMain(m *testing.M) {
	listKeys = func(string) ([]byte, error) {
		return nil, fmt.Errorf(`exec: "gnokey": executable file not found in $PATH`)
	}
	// Nor does the suite ask GitHub anything: a workspace whose origin points
	// at github.com would otherwise make a publish test depend on the network.
	repoIsPublic = func(string) (bool, error) { return false, fmt.Errorf("network disabled in tests") }
	os.Exit(m.Run())
}

// withKeys points the lookup at a fixed listing for one test, in the format
// tm2's own `gnokey list` prints (list.go: `%d. %s (%s) - addr: %v pub: %v, path: %v`).
func withKeys(t *testing.T, rows ...string) {
	t.Helper()
	listing := strings.Join(rows, "\n") + "\n"
	prev := listKeys
	t.Cleanup(func() { listKeys = prev })
	listKeys = func(string) ([]byte, error) { return []byte(listing), nil }
}

// keyRow builds one row of that listing.
func keyRow(n int, name, addr string) string {
	return fmt.Sprintf("%d. %s (local) - addr: %s pub: gpub1xxx, path: <nil>", n, name, addr)
}

// TestCreatorFromTheKeybase covers the branch CI structurally could not: an
// address that gnopm worked out by asking the keybase, rather than one it was
// handed with -addr.
func TestCreatorFromTheKeybase(t *testing.T) {
	withKeys(t,
		keyRow(0, "dev", testCreator),
		keyRow(1, "moul", "g1manfred47kzduec920z88wfr64ylksmdcedlf5"),
	)
	got, err := creatorFor("gnokey", "moul", "")
	if err != nil {
		t.Fatalf("creatorFor: %v", err)
	}
	if got != "g1manfred47kzduec920z88wfr64ylksmdcedlf5" {
		t.Errorf("creatorFor(moul) = %q, want the row that names moul", got)
	}

	// A key the keybase does not hold is an error that names where to look,
	// not a silent fallback to the first row.
	if _, err := creatorFor("gnokey", "nobody", ""); err == nil {
		t.Error("a key that is not in the listing was accepted")
	} else if !strings.Contains(err.Error(), "gnokey list") {
		t.Errorf("the error does not say where to look: %v", err)
	}
}

// TestPublishResolvesTheCreatorFromTheKeybase is the same branch end to end:
// with a keybase and no -addr, publish names the address on the key line and
// batches by dependency layer instead of falling back to one transaction per
// package.
//
// This is what #70 cost: on every machine that had a keybase this path ran and
// nothing asserted anything about it, and on CI it never ran at all.
func TestPublishResolvesTheCreatorFromTheKeybase(t *testing.T) {
	withKeys(t, keyRow(0, "moul", testCreator))
	root := txWorkspace(t) // two/v0 imports one/v0, so two layers
	f := newFakeChain(t)

	_, report, err := publishPlan(t, root, f)
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, report)
	}
	if !strings.Contains(report, "key      moul  "+testCreator) {
		t.Errorf("the key line does not name the address it resolved:\n%s", report)
	}
	if !strings.Contains(report, "2 transaction(s) for 2 package(s)") {
		t.Errorf("publish did not batch by layer with an address in hand:\n%s", report)
	}
	if strings.Contains(report, "one transaction per package") {
		t.Errorf("publish fell back as if it had found no address:\n%s", report)
	}
}

// TestPublishWithoutAKeybaseFallsBackRatherThanFailing: the other half. A
// publish that works with more prompts beats one that refuses over a name
// lookup, and it says why, once.
func TestPublishWithoutAKeybaseFallsBackRatherThanFailing(t *testing.T) {
	root := txWorkspace(t)
	f := newFakeChain(t)

	_, report, err := publishPlan(t, root, f)
	if err != nil {
		t.Fatalf("publish: %v\n%s", err, report)
	}
	if !strings.Contains(report, "one transaction per package") {
		t.Errorf("the report does not say why it is one transaction each:\n%s", report)
	}
	if !strings.Contains(report, "-addr g1...") {
		t.Errorf("the report does not say how to get the batched form back:\n%s", report)
	}
}
