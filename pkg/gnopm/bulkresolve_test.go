package gnopm

import (
	"fmt"
	"testing"
)

// What no existing test could see: the probe's tests all run against a handful
// of paths in one namespace, where one query per path and one query per
// namespace are the same number. The cost shows up at workspace size, where
// moul/gno-contracts resolves 849 paths across a dozen namespaces.

func TestBulkResolveAsksOncePerNamespace(t *testing.T) {
	f := newFakeChain(t)
	var modules []string
	for i := 0; i < bulkNamespaceMin*2; i++ {
		for _, ns := range []string{"gno.land/p/moul", "gno.land/r/moul"} {
			m := fmt.Sprintf("%s/pkg%02d/v0", ns, i)
			modules = append(modules, m)
			if i%2 == 0 {
				f.live[m] = true
			}
		}
	}
	// A neighbour whose path shares our prefix as a string, so the bulk read
	// for gno.land/p/moul returns it too. It must not end up in this
	// namespace's answer, and it must still resolve correctly on its own.
	f.live["gno.land/p/moulx/sneaky/v0"] = true

	p, err := NewProbe(probeEnv(t), modules[0], f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	after := f.count()
	if err := p.Warm(modules, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.count() - after; got != 2 {
		t.Fatalf("Warm made %d queries for %d paths in 2 namespaces, want 2",
			got, len(modules))
	}
	for i, m := range modules {
		s, err := p.State(m)
		if err != nil {
			t.Fatal(err)
		}
		want := StateAbsent
		if (i/2)%2 == 0 {
			want = StateLive
		}
		if s != want {
			t.Fatalf("State(%s) = %s, want %s", m, s, want)
		}
	}
	if s, _ := p.State("gno.land/p/moulx/sneaky/v0"); s != StateLive {
		t.Fatalf("the neighbouring namespace resolved to %s on its own", s)
	}
}

// A page that came back full might be missing entries, and a missing entry
// reads as absent, which would propose republishing a package that is already
// live. The only safe reading of a full page is "I do not know", so it falls
// back to one read per path.
func TestBulkResolveFallsBackOnATruncatedPage(t *testing.T) {
	old := bulkPathsLimit
	bulkPathsLimit = 2
	defer func() { bulkPathsLimit = old }()

	f := newFakeChain(t)
	var modules []string
	for i := 0; i < bulkNamespaceMin; i++ {
		m := fmt.Sprintf("gno.land/p/moul/pkg%02d/v0", i)
		modules = append(modules, m)
		f.live[m] = true
	}
	p, err := NewProbe(probeEnv(t), modules[0], f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	after := f.count()
	if err := p.Warm(modules, nil); err != nil {
		t.Fatal(err)
	}
	// One truncated vm/qpaths, then one vm/qfile per path.
	if got := f.count() - after; got != 1+len(modules) {
		t.Fatalf("Warm made %d queries, want 1 truncated bulk read plus %d per-path reads",
			got, len(modules))
	}
	for _, m := range modules {
		if s, _ := p.State(m); s != StateLive {
			t.Fatalf("State(%s) = %s after the fallback, want live", m, s)
		}
	}
}

// A lone import from a stranger's namespace is the common case, and a whole
// namespace listing costs about four per-path reads, so below the threshold
// the bulk call is a pessimisation.
func TestBulkResolveSkipsNamespacesTooSmallToPayFor(t *testing.T) {
	f := newFakeChain(t)
	var modules []string
	for i := 0; i < bulkNamespaceMin-1; i++ {
		m := fmt.Sprintf("gno.land/p/stranger/pkg%02d/v0", i)
		modules = append(modules, m)
		f.live[m] = true
	}
	p, err := NewProbe(probeEnv(t), "gno.land/p/stranger/pkg00/v0", f.srv.URL, "test-1")
	if err != nil {
		t.Fatal(err)
	}
	after := f.count()
	if err := p.Warm(modules, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.count() - after; got != len(modules) {
		t.Fatalf("Warm made %d queries for %d path(s), want one each", got, len(modules))
	}
}

func TestNamespacePrefix(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"gno.land/p/moul/md/v0", "gno.land/p/moul"},
		{"gno.land/r/moul/x/daily/wordle/v0", "gno.land/r/moul"},
		{"gno.land/r/g1abc/thing/v0", "gno.land/r/g1abc"},
		{"gno.land/p/moul", ""},
		{"", ""},
	} {
		if got := namespacePrefix(tt.in); got != tt.want {
			t.Errorf("namespacePrefix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
