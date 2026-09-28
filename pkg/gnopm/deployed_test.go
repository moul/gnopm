package gnopm

import (
	"bytes"
	"strings"
	"testing"
)

const deployedBody = "package a\n\nfunc A() string { return \"committed\" }\n"

// deployedRepo is a workspace with one package, and a chain serving a copy.
func deployedRepo(t *testing.T, onChain string) (string, *fakeChain) {
	t.Helper()
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", deployedBody)
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	f := newFakeChain(t)
	f.live["gno.land/p/a/v0"] = true
	if onChain != "" {
		f.files = map[string]string{
			"gno.land/p/a/v0/gnomod.toml": "module = \"gno.land/p/a/v0\"\ngno = \"0.9\"\n",
			"gno.land/p/a/v0/a.gno":       onChain,
		}
	}
	t.Setenv(cacheEnv, t.TempDir())
	return root, f
}

func runDeployed(t *testing.T, root string, f *fakeChain) (string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	err := Run([]string{"verify", "-C", root, "-deployed", "-rpc", f.srv.URL, "-chainid", "test"}, &out, &errb)
	return out.String(), errb.String(), err
}

// TestVerifyDeployedMatches is the good case, and the one that proves the two
// hashes are computed over the same file set. If they were not, this would fail
// for every package and the command would be useless rather than wrong in an
// interesting way.
func TestVerifyDeployedMatches(t *testing.T) {
	root, f := deployedRepo(t, deployedBody)

	_, errb, err := runDeployed(t, root, f)
	if err != nil {
		t.Fatalf("a package that matches was reported as differing: %v\n%s", err, errb)
	}
	if !strings.Contains(errb, "1 live, 1 match, 0 differ") {
		t.Errorf("unexpected summary:\n%s", errb)
	}
}

// TestVerifyDeployedCatchesADrift is the failure the command exists to find: a
// package edited in the tree after it shipped. A published path cannot be
// redefined, so the chain will never catch up and only a new version fixes it.
func TestVerifyDeployedCatchesADrift(t *testing.T) {
	root, f := deployedRepo(t, "package a\n\nfunc A() string { return \"deployed long ago\" }\n")

	_, errb, err := runDeployed(t, root, f)
	if err == nil {
		t.Fatalf("drift was not reported:\n%s", errb)
	}
	if !strings.Contains(errb, "differs") || !strings.Contains(errb, "gno.land/p/a/v0") {
		t.Errorf("the report does not name the package:\n%s", errb)
	}
	// Both hashes, because "they differ" without saying how is not actionable.
	if strings.Count(errb, hashPrefix) < 2 {
		t.Errorf("the report does not show both hashes:\n%s", errb)
	}
	if !strings.Contains(err.Error(), "new version") {
		t.Errorf("the error does not say what fixes it: %v", err)
	}
}

// TestVerifyDeployedSkipsWhatIsNotLive. Absent is the normal state of something
// not published yet and is not a failure; parked is a submission still waiting
// on an approver, so there are no deployed bytes to compare against either.
func TestVerifyDeployedSkipsWhatIsNotLive(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", deployedBody)
	addPkg(t, root, "p/b", "gno.land/p/b/v0", "package b\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	f := newFakeChain(t)
	f.parked["gno.land/p/b/v0"] = true
	t.Setenv(cacheEnv, t.TempDir())

	_, errb, err := runDeployed(t, root, f)
	if err != nil {
		t.Fatalf("an unpublished workspace failed: %v\n%s", err, errb)
	}
	if !strings.Contains(errb, "0 live") || !strings.Contains(errb, "1 parked") {
		t.Errorf("unexpected summary:\n%s", errb)
	}
}

// TestHashPayloadIsWhatAddpkgUploads is the assertion the whole command rests
// on: the tree side must hash exactly the file set publish sends, not the
// git-tracked set. Comparing the wrong set would make every live package look
// like it had drifted, which is a false alarm on the one check people would
// most want to trust.
//
// The first version of this test used README.md as the un-uploaded file and
// failed: .md IS uploaded, and gno.land renders it. The comment in deployed.go
// said so too, and was wrong with it.
func TestHashPayloadIsWhatAddpkgUploads(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", deployedBody)
	before, err := hashPayload(root + "/p/a")
	if err != nil {
		t.Fatal(err)
	}

	// Tracked by git, never uploaded: uploadable() takes .gno, .toml, .md and
	// a few licence names, and nothing else.
	write(t, root+"/p/a/Makefile", "all:\n\techo hi\n")
	write(t, root+"/p/a/logo.png", "not really a png")
	after, err := hashPayload(root + "/p/a")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("an un-uploaded file changed the payload hash:\n%s\n%s", before, after)
	}

	// And one that IS uploaded has to move it, or the test above would pass
	// against a hash that ignores everything.
	write(t, root+"/p/a/README.md", "# uploaded, and rendered on gno.land\n")
	withDoc, err := hashPayload(root + "/p/a")
	if err != nil {
		t.Fatal(err)
	}
	if withDoc == before {
		t.Error("an uploaded file did not change the payload hash")
	}
}
