package gnopm

import (
	"bytes"
	"strings"
	"testing"
)

func searchChain(t *testing.T) *fakeChain {
	t.Helper()
	f := newFakeChain(t)
	for _, p := range []string{
		"gno.land/p/moul/md/v0",
		"gno.land/p/moul/md/v1",
		"gno.land/r/moul/home",
		"gno.land/p/demo/avl/v0",
		"gno.land/r/demo/boards/v0",
	} {
		f.live[p] = true
	}
	return f
}

func runSearch(t *testing.T, f *fakeChain, args ...string) (string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	full := append([]string{"search", "-rpc", f.srv.URL, "-chainid", "test"}, args...)
	if err := Run(full, &out, &errb); err != nil {
		t.Fatalf("search %v: %v\n%s", args, err, errb.String())
	}
	return out.String(), errb.String()
}

// TestSearchByPrefixAndWord covers the two shapes that differ in where the
// filtering happens: a path prefix the chain resolves, and a bare word scanned
// over one page because a word is a prefix of nothing.
func TestSearchByPrefixAndWord(t *testing.T) {
	f := searchChain(t)

	out, _ := runSearch(t, f, "gno.land/p/moul")
	if !strings.Contains(out, "gno.land/p/moul/md/v0") || !strings.Contains(out, "gno.land/p/moul/md/v1") {
		t.Errorf("prefix search missed a path:\n%s", out)
	}
	if strings.Contains(out, "gno.land/p/demo/avl") {
		t.Errorf("prefix search returned something outside the prefix:\n%s", out)
	}

	out, _ = runSearch(t, f, "avl")
	if !strings.Contains(out, "gno.land/p/demo/avl/v0") {
		t.Errorf("word search missed the obvious hit:\n%s", out)
	}
	if strings.Contains(out, "gno.land/p/moul/md") {
		t.Errorf("word search returned a non-match:\n%s", out)
	}

	// The kind column is what makes a list of paths scannable.
	if !strings.Contains(out, "package") {
		t.Errorf("no kind column:\n%s", out)
	}
	out, _ = runSearch(t, f, "boards")
	if !strings.Contains(out, "realm") {
		t.Errorf("a realm was not labelled as one:\n%s", out)
	}
}

// TestSearchSendsTheLimitWhereTheChainReadsIt. vm/qpaths takes ?limit= on the
// ABCI path, not in the data (gno's pathsLimit cuts on "?"), so a limit put in
// the wrong place is silently ignored and the node serves its default page.
// That failure is invisible from the output, which is why it is asserted here.
func TestSearchSendsTheLimitWhereTheChainReadsIt(t *testing.T) {
	f := searchChain(t)
	out, _ := runSearch(t, f, "-limit", "1", "gno.land/")
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); n > 2 {
		// One header line plus at most one row.
		t.Errorf("the limit did not reach the chain, got %d lines:\n%s", n, out)
	}
}

// TestSearchEmptyAnswerPipesAsEmpty, like every other view here: nothing on
// stdout, the explanation on stderr.
func TestSearchEmptyAnswerPipesAsEmpty(t *testing.T) {
	f := searchChain(t)
	out, errb := runSearch(t, f, "nothingmatchesthis")
	if out != "" {
		t.Errorf("an empty answer wrote to stdout: %q", out)
	}
	if !strings.Contains(errb, "nothing on") {
		t.Errorf("no explanation on stderr:\n%s", errb)
	}
}

// TestSearchWorksOutsideAWorkspace: finding a package is what you do before you
// have one, so requiring a gnowork.toml would put the command behind the
// problem it helps solve.
func TestSearchWorksOutsideAWorkspace(t *testing.T) {
	f := searchChain(t)
	dir := t.TempDir()
	var out, errb bytes.Buffer
	err := Run([]string{"search", "-C", dir, "-rpc", f.srv.URL, "-chainid", "test", "gno.land/p/moul"}, &out, &errb)
	if err != nil {
		t.Fatalf("search outside a workspace: %v\n%s", err, errb.String())
	}
	if !strings.Contains(out.String(), "gno.land/p/moul/md/v0") {
		t.Errorf("no results outside a workspace:\n%s", out.String())
	}
}
