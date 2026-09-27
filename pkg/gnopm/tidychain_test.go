package gnopm

import (
	"bytes"
	"strings"
	"testing"
)

// chainTidyRepo is a workspace importing something it cannot resolve.
func chainTidyRepo(t *testing.T) (string, *fakeChain, string) {
	t.Helper()
	root := newRepo(t)
	addPkg(t, root, "p/me/app", "gno.land/p/me/app/v0",
		"package app\n\nimport \"gno.land/p/nt/tinyavl/v0\"\n\nfunc A() string { return tinyavl.Get(\"hi\") }\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	f := newFakeChain(t)
	f.files = map[string]string{
		"gno.land/p/nt/tinyavl/v0/gnomod.toml": "module = \"gno.land/p/nt/tinyavl/v0\"\ngno = \"0.9\"\n",
		"gno.land/p/nt/tinyavl/v0/tinyavl.gno": "package tinyavl\n\nfunc Get(k string) string { return k }\n",
	}
	f.live["gno.land/p/nt/tinyavl/v0"] = true
	f.live["gno.land/p/me/app/v0"] = true
	cache := t.TempDir()
	t.Setenv(cacheEnv, cache)
	return root, f, cache
}

func runTidy(t *testing.T, root string, f *fakeChain, extra ...string) (string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	args := append([]string{"tidy", "-C", root, "-rpc", f.srv.URL, "-chainid", "test"}, extra...)
	err := Run(args, &out, &errb)
	return out.String(), errb.String(), err
}

// TestTidyFetchesAnUnresolvedImport is the `go mod tidy` half: an import that
// resolves to nothing in the workspace, nothing in the lock, and is live on the
// chain its path names.
func TestTidyFetchesAnUnresolvedImport(t *testing.T) {
	root, f, _ := chainTidyRepo(t)

	_, errb, err := runTidy(t, root, f)
	if err != nil {
		t.Fatalf("tidy: %v\n%s", err, errb)
	}
	en, ok := mustByModule(t, root)["gno.land/p/nt/tinyavl/v0"]
	if !ok {
		t.Fatalf("tidy did not add the unresolved import:\n%s", errb)
	}
	if en.Source.Variant() != "chain" {
		t.Errorf("added as %q, want chain: %+v", en.Source.Variant(), en.Source)
	}
	mustRun(t, root, "verify")
	if s := mustRun(t, root, "status"); !strings.Contains(s, "up to date") {
		t.Fatalf("tidy left the workspace stale:\n%s", s)
	}
}

// TestTidyWillNotFetchWhatIsNotLive. A parked path is a submission an approver
// can still reject, so recording its hash would pin bytes that may never be
// live; an absent one is a typo, and inventing an entry turns a clear "cannot
// resolve" into a lock that fails later and further away.
func TestTidyWillNotFetchWhatIsNotLive(t *testing.T) {
	root, f, _ := chainTidyRepo(t)
	delete(f.live, "gno.land/p/nt/tinyavl/v0")
	f.parked["gno.land/p/nt/tinyavl/v0"] = true

	_, errb, err := runTidy(t, root, f)
	if err != nil {
		t.Fatalf("tidy: %v\n%s", err, errb)
	}
	if _, ok := mustByModule(t, root)["gno.land/p/nt/tinyavl/v0"]; ok {
		t.Error("tidy recorded a parked path")
	}
	if !strings.Contains(errb, "parked") || !strings.Contains(errb, "reject") {
		t.Errorf("tidy did not explain why it declined:\n%s", errb)
	}

	// And an absent one is named rather than silently ignored. It needs a
	// chain that serves nothing for the path: leaving the files in place would
	// make it genuinely live, since serving a package IS what live means.
	root2 := newRepo(t)
	addPkg(t, root2, "p/me/app", "gno.land/p/me/app/v0",
		"package app\n\nimport \"gno.land/p/nt/tinyavl/v0\"\n\nfunc A() string { return tinyavl.Get(\"hi\") }\n")
	commit(t, root2, "initial")
	mustRun(t, root2, "sync")
	f2 := newFakeChain(t)
	f2.live["gno.land/p/me/app/v0"] = true
	t.Setenv(cacheEnv, t.TempDir())

	_, errb2, err := runTidy(t, root2, f2)
	if err != nil {
		t.Fatalf("tidy: %v\n%s", err, errb2)
	}
	if !strings.Contains(errb2, "resolves nowhere") {
		t.Errorf("an unresolvable import went unmentioned:\n%s", errb2)
	}
}

// TestTidyDropsAnUnusedChainDependencyForTheRightReason.
//
// The action was already right, because a chain entry has no commit and so
// failed the git-reachability test by accident. The justification printed was
// nonsense: "it never reached origin/main" about a package that lives on a
// chain and has nothing to do with this repository's history.
//
// Its real rule is stronger than the git one: dropping loses nothing at all,
// because the bytes are on a chain that cannot delete or redefine them.
func TestTidyDropsAnUnusedChainDependencyForTheRightReason(t *testing.T) {
	root, f, _ := chainTidyRepo(t)
	// An upstream ref, so the git branch of the rule is live and would produce
	// its own message if the chain entry fell through to it.
	gitCmd(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")

	if _, errb, err := runTidy(t, root, f); err != nil {
		t.Fatalf("tidy: %v\n%s", err, errb)
	}
	// Now stop importing it.
	write(t, root+"/p/me/app/app.gno", "package app\n\nfunc A() string { return \"a\" }\n")
	commit(t, root, "drop the import")

	_, errb, err := runTidy(t, root, f)
	if err != nil {
		t.Fatalf("tidy: %v\n%s", err, errb)
	}
	if _, ok := mustByModule(t, root)["gno.land/p/nt/tinyavl/v0"]; ok {
		t.Errorf("an unimported chain dependency was kept:\n%s", errb)
	}
	if strings.Contains(errb, "never reached") {
		t.Errorf("a chain dependency was dropped on a git argument:\n%s", errb)
	}
	if !strings.Contains(errb, "brings it back") {
		t.Errorf("the drop did not say why it is safe:\n%s", errb)
	}
}

// TestTidyDryRunFetchesNothing: -n changes nothing, including not writing a
// lock entry for something it would have fetched.
func TestTidyDryRunFetchesNothing(t *testing.T) {
	root, f, _ := chainTidyRepo(t)
	before := read(t, root+"/"+lockFile)

	_, errb, err := runTidy(t, root, f, "-n")
	if err != nil {
		t.Fatalf("tidy -n: %v\n%s", err, errb)
	}
	if !strings.Contains(errb, "would get") {
		t.Errorf("-n did not say what it would fetch:\n%s", errb)
	}
	if read(t, root+"/"+lockFile) != before {
		t.Error("-n wrote the lock")
	}
}
