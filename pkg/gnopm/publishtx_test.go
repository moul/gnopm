package gnopm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCreator = "g1jg8mtutu9khhfwc4nxmuhcpftf0pajdhfvsqf5"

// txWorkspace is two packages where one imports the other, so the document has
// to carry them in dependency order.
func txWorkspace(t *testing.T) string {
	t.Helper()
	root := newRepo(t)
	addPkg(t, root, "p/one", "gno.land/p/moul/one/v0", "package one\n\nfunc One() int { return 1 }\n")
	addPkg(t, root, "r/two", "gno.land/r/moul/two/v0",
		"package two\n\nimport \"gno.land/p/moul/one/v0\"\n\nfunc Two() int { return one.One() + 1 }\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	return root
}

// TestPublishTxDocument: the whole deploy as one document, signed once.
//
// The shape asserted here is the one upstream signs and broadcasts in its own
// CI (gno.land/pkg/integration/testdata/addpkg_multi_msg.txtar), so this test
// is pinning fidelity to that fixture rather than to gnopm's own opinion.
func TestPublishTxDocument(t *testing.T) {
	root := txWorkspace(t)
	f := newFakeChain(t)
	out := filepath.Join(t.TempDir(), "tx.json")

	var stdout, stderr bytes.Buffer
	err := Run([]string{"publish", "-C", root, "-rpc", f.srv.URL, "-chainid", "test-1",
		"-key", "moul", "-addr", testCreator, "-o", out}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("publish -o: %v\n%s", err, stderr.String())
	}

	var doc struct {
		Msgs []struct {
			Type    string `json:"@type"`
			Creator string `json:"creator"`
			Package struct {
				Name  string `json:"name"`
				Path  string `json:"path"`
				Files []struct {
					Name string `json:"name"`
					Body string `json:"body"`
				} `json:"files"`
			} `json:"package"`
			MaxDeposit string `json:"max_deposit"`
		} `json:"msg"`
		Fee struct {
			GasWanted string `json:"gas_wanted"`
			GasFee    string `json:"gas_fee"`
		} `json:"fee"`
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the document is not JSON: %v\n%s", err, b)
	}

	if len(doc.Msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(doc.Msgs))
	}
	// Dependency order, inside one transaction. The messages execute in order,
	// so an import that goes up after its dependent would fail the whole tx.
	if doc.Msgs[0].Package.Path != "gno.land/p/moul/one/v0" || doc.Msgs[1].Package.Path != "gno.land/r/moul/two/v0" {
		t.Fatalf("out of dependency order: %s then %s", doc.Msgs[0].Package.Path, doc.Msgs[1].Package.Path)
	}
	for _, m := range doc.Msgs {
		if m.Type != "/vm.m_addpkg" {
			t.Fatalf("wrong amino type %q", m.Type)
		}
		if m.Creator != testCreator {
			t.Fatalf("wrong creator %q", m.Creator)
		}
		if m.MaxDeposit == "" || !strings.HasSuffix(m.MaxDeposit, "ugnot") {
			t.Fatalf("max_deposit is %q", m.MaxDeposit)
		}
		if len(m.Package.Files) != 2 {
			t.Fatalf("%s carries %d files, want gnomod.toml and the source", m.Package.Path, len(m.Package.Files))
		}
	}
	// The package name is the `package` clause, not the last path element:
	// gno.land/p/moul/one/v0 declares `one`, and getting this wrong deploys a
	// package the VM cannot resolve.
	if doc.Msgs[0].Package.Name != "one" || doc.Msgs[1].Package.Name != "two" {
		t.Fatalf("package names are %q and %q", doc.Msgs[0].Package.Name, doc.Msgs[1].Package.Name)
	}
	// Bodies, not paths: the chain never sees the working tree.
	var found bool
	for _, fl := range doc.Msgs[0].Package.Files {
		if fl.Name == "one.gno" && strings.Contains(fl.Body, "func One() int") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the source body did not travel: %+v", doc.Msgs[0].Package.Files)
	}
	if doc.Fee.GasWanted == "" || !strings.HasSuffix(doc.Fee.GasFee, "ugnot") {
		t.Fatalf("fee is %+v", doc.Fee)
	}

	// The commands are data and go to stdout, so the whole thing still pipes.
	script := stdout.String()
	for _, want := range []string{
		"gnokey sign",
		"-tx-path '" + out + "'",
		"-chainid test-1",
		"-account-number 7",    // from the chain
		"-account-sequence 42", // from the chain
		"gnokey broadcast",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("the emitted commands are missing %q:\n%s", want, script)
		}
	}
	// Numbers without this caveat would be worse than no numbers: they are
	// part of the signature.
	if !strings.Contains(stderr.String(), "stops being valid") {
		t.Fatalf("the report does not warn about the sequence:\n%s", stderr.String())
	}
}

// TestPublishTxNeedsAnAddress: the creator is a field of every message, so
// gnopm cannot write anything without it, and it will not read a keybase to
// find out.
func TestPublishTxNeedsAnAddress(t *testing.T) {
	root := txWorkspace(t)
	f := newFakeChain(t)
	out := filepath.Join(t.TempDir(), "tx.json")

	var stdout, stderr bytes.Buffer
	err := Run([]string{"publish", "-C", root, "-rpc", f.srv.URL, "-chainid", "test-1",
		"-key", "moul", "-o", out}, &stdout, &stderr)
	if err == nil {
		t.Fatal("-o without an address was accepted")
	}
	if !strings.Contains(err.Error(), "gnokey list") {
		t.Fatalf("the error does not say where to find it: %v", err)
	}

	// A key name in -addr is the same mistake spelled differently.
	err = Run([]string{"publish", "-C", root, "-rpc", f.srv.URL, "-chainid", "test-1",
		"-addr", "moul", "-o", out}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "does not look like an address") {
		t.Fatalf("a key name in -addr was accepted: %v", err)
	}

	// An address in -key is enough on its own: it is unambiguous.
	stdout.Reset()
	stderr.Reset()
	if err := Run([]string{"publish", "-C", root, "-rpc", f.srv.URL, "-chainid", "test-1",
		"-key", testCreator, "-o", out}, &stdout, &stderr); err != nil {
		t.Fatalf("an address in -key was refused: %v\n%s", err, stderr.String())
	}
}

// TestPublishTxUnknownAccount: a chain that will not say the account number is
// not a reason to refuse to write the document, but it is a reason to say so
// and leave the numbers as placeholders rather than guess zero.
func TestPublishTxUnknownAccount(t *testing.T) {
	root := txWorkspace(t)
	f := newFakeChain(t)
	f.noAccount = true
	out := filepath.Join(t.TempDir(), "tx.json")

	var stdout, stderr bytes.Buffer
	if err := Run([]string{"publish", "-C", root, "-rpc", f.srv.URL, "-chainid", "test-1",
		"-addr", testCreator, "-o", out}, &stdout, &stderr); err != nil {
		t.Fatalf("publish -o: %v\n%s", err, stderr.String())
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("the document was not written: %v", err)
	}
	if !strings.Contains(stdout.String(), "<account-number>") {
		t.Fatalf("a missing account number was filled in anyway:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "query auth/accounts/") {
		t.Fatalf("the report does not say how to get it:\n%s", stderr.String())
	}
}

// TestPayloadFoldsInFiletests is the bug this change had to fix before it
// could write a document at all.
//
// gno's ReadMemPackage folds *_filetest.gno in from a filetests/ subdirectory
// (gnovm/pkg/gnolang/mempackage.go), so those bytes are uploaded and charged
// for. gnopm skipped every subdirectory, which under-counted the payload, so
// gas, fee and deposit were sized from fewer bytes than the transaction
// carries, and a document built from that list would not have been the package
// gnokey sends. No existing test could see it: they all used flat packages.
func TestPayloadFoldsInFiletests(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "home.gno"), "package home\n")
	write(t, filepath.Join(dir, "gnomod.toml"), "module = \"x\"\n")
	write(t, filepath.Join(dir, "filetests", "z_ok_filetest.gno"), "package main\n\nfunc main() {}\n")
	// Only *_filetest.gno travels from there, and no other subdirectory does.
	write(t, filepath.Join(dir, "filetests", "helper.gno"), "package main\n")
	write(t, filepath.Join(dir, "content", "bio.md"), "never counted\n")

	files, total, err := Payload(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	want := map[string]bool{"home.gno": true, "gnomod.toml": true, "z_ok_filetest.gno": true}
	if len(files) != len(want) {
		t.Fatalf("payload = %v, want %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Fatalf("payload = %v, want %v", names, want)
		}
	}
	// bodies 13+13+29 = 55; names 8+11+17 = 36
	if total != 91 {
		t.Fatalf("total = %d, want 91", total)
	}

	// And the file the message carries is the flat base name, not the path:
	// the toolchain writes a filetest back into filetests/ on the way out.
	msg, err := AddPackageFor(dir, "gno.land/p/x/home/v0", testCreator, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range msg.Package.Files {
		if strings.Contains(f.Name, "/") {
			t.Fatalf("a message file name is a path: %q", f.Name)
		}
	}
	// A filetest declares `package main` and a _test.gno may declare
	// `foo_test`, so neither may name the package.
	if msg.Package.Name != "home" {
		t.Fatalf("package name is %q, want home", msg.Package.Name)
	}
}

// TestBatchDocuments: a deploy larger than one transaction has to be split,
// and the split has to keep dependency order, which is what makes batching
// gnopm's problem rather than the user's.
func TestBatchDocuments(t *testing.T) {
	big := strings.Repeat("x", maxTxBytes/3)
	var msgs []AddPackageMsg
	for i := 0; i < 5; i++ {
		msgs = append(msgs, AddPackageMsg{
			Type:    addPackageType,
			Creator: testCreator,
			Package: &TxMemPkg{
				Name:  "p",
				Path:  fmt.Sprintf("gno.land/p/x/p%d/v0", i),
				Files: []TxMemFile{{Name: "p.gno", Body: big}},
			},
		})
	}
	docs, err := batchDocuments(msgs, func(AddPackageMsg) int64 { return 1000 })
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) < 2 {
		t.Fatalf("5 messages of %d bytes fitted in %d document(s)", len(big), len(docs))
	}
	var order []string
	for _, d := range docs {
		b, err := d.JSON()
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > maxTxBytes {
			t.Fatalf("a batch is %d bytes, over the %d limit", len(b), maxTxBytes)
		}
		if d.Fee.GasWanted == "" {
			t.Fatal("a batch carries no fee")
		}
		for _, m := range d.Msgs {
			order = append(order, m.Package.Path)
		}
	}
	for i, p := range order {
		if p != fmt.Sprintf("gno.land/p/x/p%d/v0", i) {
			t.Fatalf("batching reordered the messages: %v", order)
		}
	}

	// One package that cannot fit in any transaction is worth saying rather
	// than writing a document the mempool will reject.
	huge := []AddPackageMsg{{
		Type: addPackageType, Creator: testCreator,
		Package: &TxMemPkg{Name: "p", Path: "gno.land/p/x/huge/v0",
			Files: []TxMemFile{{Name: "p.gno", Body: strings.Repeat("y", maxTxBytes+1)}}},
	}}
	if _, err := batchDocuments(huge, func(AddPackageMsg) int64 { return 1 }); err == nil {
		t.Fatal("a package larger than a transaction was batched anyway")
	} else if !strings.Contains(err.Error(), "cannot be deployed in one message") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// TestPublishTxBatchesToNumberedFiles covers the naming and the per-batch
// sequence, which is the part a user gets wrong by hand.
func TestPublishTxBatchesToNumberedFiles(t *testing.T) {
	root := newRepo(t)
	// Three packages, each a third of a transaction, so they cannot share one.
	body := "package big\n\nconst Blob = \"" + strings.Repeat("z", maxTxBytes/3) + "\"\n"
	for _, n := range []string{"a", "b", "c"} {
		addPkg(t, root, "p/"+n, "gno.land/p/moul/"+n+"/v0", body)
	}
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	f := newFakeChain(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "tx.json")
	var stdout, stderr bytes.Buffer
	if err := Run([]string{"publish", "-C", root, "-rpc", f.srv.URL, "-chainid", "test-1",
		"-addr", testCreator, "-o", out}, &stdout, &stderr); err != nil {
		t.Fatalf("publish -o: %v\n%s", err, stderr.String())
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a batched deploy wrote the unnumbered name too")
	}
	for i := 1; i <= 2; i++ {
		if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("tx.%d.json", i))); err != nil {
			t.Fatalf("batch %d missing: %v", i, err)
		}
	}
	// Each batch takes the next sequence, because each is its own signature.
	for _, want := range []string{"-account-sequence 42", "-account-sequence 43"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, stdout.String())
		}
	}
}
