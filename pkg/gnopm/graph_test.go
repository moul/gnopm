package gnopm

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// graphRepo builds the shape every projection has to get right: a package with
// two versions where the new one imports the old, a realm importing the new
// one, and one import that leaves the workspace entirely.
func graphRepo(t *testing.T) string {
	t.Helper()
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n\nfunc A() string { return \"v0\" }\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	mustRun(t, root, "bump", "p/a")

	// v1 delegates to v0, which is the whole reason a superseded version has
	// to stay resolvable and therefore has to be in the graph.
	write(t, filepath.Join(root, "p/a/a.gno"),
		"package a\n\nimport \"gno.land/p/a/v0\"\n\nfunc A() string { return a.A() + \"+v1\" }\n")
	addPkg(t, root, "r/b", "gno.land/r/b/v0",
		"package b\n\nimport (\n\t\"gno.land/p/a/v1\"\n\t\"gno.land/p/demo/ufmt\"\n)\n")
	commit(t, root, "v1 delegates to v0, and a realm uses it")
	mustRun(t, root, "sync")
	return root
}

func modulesOf(g *Graph) []string {
	var out []string
	for _, n := range g.Nodes {
		out = append(out, n.Module)
	}
	sort.Strings(out)
	return out
}

func edgesOf(g *Graph) []string {
	var out []string
	for _, e := range g.Edges {
		out = append(out, e.From+" -> "+e.To)
	}
	sort.Strings(out)
	return out
}

func TestGraphProjections(t *testing.T) {
	root := graphRepo(t)
	e := testEnv(root, new(bytes.Buffer))

	for _, tc := range []struct {
		name  string
		opts  GraphOptions
		nodes []string
		edges []string
	}{
		{
			name: "whole workspace",
			nodes: []string{
				"gno.land/p/a/v0", "gno.land/p/a/v1",
				"gno.land/p/demo/ufmt", "gno.land/r/b/v0",
			},
			edges: []string{
				"gno.land/p/a/v1 -> gno.land/p/a/v0",
				"gno.land/r/b/v0 -> gno.land/p/a/v1",
				"gno.land/r/b/v0 -> gno.land/p/demo/ufmt",
			},
		},
		{
			// The README graph: one node per package, and the v1 -> v0 edge
			// becomes a self-edge, which is noise rather than information.
			name: "latest only",
			opts: GraphOptions{Latest: true},
			nodes: []string{
				"gno.land/p/a/v1", "gno.land/p/demo/ufmt", "gno.land/r/b/v0",
			},
			edges: []string{
				"gno.land/r/b/v0 -> gno.land/p/a/v1",
				"gno.land/r/b/v0 -> gno.land/p/demo/ufmt",
			},
		},
		{
			// A module nothing here contains cannot be bumped or published
			// from here, so there is a picture that leaves it out.
			name:  "internal only",
			opts:  GraphOptions{Internal: true},
			nodes: []string{"gno.land/p/a/v0", "gno.land/p/a/v1", "gno.land/r/b/v0"},
			edges: []string{
				"gno.land/p/a/v1 -> gno.land/p/a/v0",
				"gno.land/r/b/v0 -> gno.land/p/a/v1",
			},
		},
		{
			// A named package is a neighbourhood in both directions: what it
			// needs, and what needs it. r/b reaches v0 only through v1, so a
			// one-hop answer would have been wrong.
			name: "rooted, both directions",
			opts: GraphOptions{Root: "gno.land/p/a/v0"},
			nodes: []string{
				"gno.land/p/a/v0", "gno.land/p/a/v1", "gno.land/r/b/v0",
			},
			edges: []string{
				"gno.land/p/a/v1 -> gno.land/p/a/v0",
				"gno.land/r/b/v0 -> gno.land/p/a/v1",
			},
		},
		{
			name:  "rooted, dependencies only",
			opts:  GraphOptions{Root: "gno.land/p/a/v0", Deps: true},
			nodes: []string{"gno.land/p/a/v0"},
		},
		{
			name: "rooted, dependents only",
			opts: GraphOptions{Root: "gno.land/p/a/v0", Dependents: true},
			nodes: []string{
				"gno.land/p/a/v0", "gno.land/p/a/v1", "gno.land/r/b/v0",
			},
			edges: []string{
				"gno.land/p/a/v1 -> gno.land/p/a/v0",
				"gno.land/r/b/v0 -> gno.land/p/a/v1",
			},
		},
		{
			name: "rooted at the realm, dependencies only",
			opts: GraphOptions{Root: "gno.land/r/b/v0", Deps: true},
			nodes: []string{
				"gno.land/p/a/v0", "gno.land/p/a/v1",
				"gno.land/p/demo/ufmt", "gno.land/r/b/v0",
			},
			edges: []string{
				"gno.land/p/a/v1 -> gno.land/p/a/v0",
				"gno.land/r/b/v0 -> gno.land/p/a/v1",
				"gno.land/r/b/v0 -> gno.land/p/demo/ufmt",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := BuildGraph(e, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := modulesOf(g); strings.Join(got, ",") != strings.Join(tc.nodes, ",") {
				t.Fatalf("nodes:\n got %v\nwant %v", got, tc.nodes)
			}
			if got := edgesOf(g); strings.Join(got, ",") != strings.Join(tc.edges, ",") {
				t.Fatalf("edges:\n got %v\nwant %v", got, tc.edges)
			}
		})
	}
}

// TestGraphNodeKinds: the picture is only useful if a node says what it is.
func TestGraphNodeKinds(t *testing.T) {
	root := graphRepo(t)
	g, err := BuildGraph(testEnv(root, new(bytes.Buffer)), GraphOptions{})
	if err != nil {
		t.Fatal(err)
	}
	byModule := map[string]GraphNode{}
	for _, n := range g.Nodes {
		byModule[n.Module] = n
	}
	// A version with no directory left, which is the state the whole tool
	// exists to make survivable.
	if n := byModule["gno.land/p/a/v0"]; n.Source != "commit" || n.Commit == "" || n.External {
		t.Fatalf("the pinned version is not marked as one: %+v", n)
	}
	if n := byModule["gno.land/p/a/v1"]; n.Source != "dir" || n.Dir != "p/a" || n.Kind != "p" {
		t.Fatalf("the tree version is wrong: %+v", n)
	}
	if n := byModule["gno.land/r/b/v0"]; n.Kind != "r" {
		t.Fatalf("a realm is not marked as one: %+v", n)
	}
	if n := byModule["gno.land/p/demo/ufmt"]; !n.External || n.Source != "" {
		t.Fatalf("an import from outside the workspace is not marked external: %+v", n)
	}

	dot := g.DOT()
	for _, want := range []string{
		"digraph gnopm {",
		`"gno.land/p/a/v1" -> "gno.land/p/a/v0";`,
		`label="p/a/v1"`,
		`tooltip="gno.land/p/demo/ufmt"`,
	} {
		if !strings.Contains(dot, want) {
			t.Fatalf("DOT is missing %q:\n%s", want, dot)
		}
	}
}

// TestGraphOutput pins the stream discipline, which is what makes
// `gnopm graph | dot -Tsvg` work: DOT on stdout and the count on stderr.
func TestGraphOutput(t *testing.T) {
	root := graphRepo(t)

	var out, errw bytes.Buffer
	if err := Run([]string{"graph", "-C", root}, &out, &errw); err != nil {
		t.Fatalf("graph: %v\n%s", err, errw.String())
	}
	if !strings.HasPrefix(out.String(), "digraph gnopm {") {
		t.Fatalf("stdout does not start with DOT:\n%s", out.String())
	}
	if strings.Contains(out.String(), "node(s)") {
		t.Fatal("the count reached stdout, so the pipe gets more than DOT")
	}
	if !strings.Contains(errw.String(), "node(s)") {
		t.Fatalf("the count is not on stderr: %q", errw.String())
	}

	out.Reset()
	errw.Reset()
	if err := Run([]string{"graph", "-C", root, "-json"}, &out, &errw); err != nil {
		t.Fatal(err)
	}
	var g Graph
	if err := json.Unmarshal(out.Bytes(), &g); err != nil {
		t.Fatalf("-json is not a graph: %v\n%s", err, out.String())
	}
	if len(g.Nodes) != 4 || len(g.Edges) != 3 {
		t.Fatalf("-json has %d nodes and %d edges, want 4 and 3", len(g.Nodes), len(g.Edges))
	}
}

// TestGraphChanged covers the projection that needs git rather than the lock.
func TestGraphChanged(t *testing.T) {
	root := graphRepo(t)
	base, err := git(root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	base = strings.TrimSpace(base)

	write(t, filepath.Join(root, "r/b/b.gno"),
		"package b\n\nimport (\n\t\"gno.land/p/a/v1\"\n\t\"gno.land/p/demo/ufmt\"\n)\n\nfunc B() {}\n")
	commit(t, root, "touch the realm")

	g, err := BuildGraph(testEnv(root, new(bytes.Buffer)), GraphOptions{Changed: base})
	if err != nil {
		t.Fatal(err)
	}
	// The touched package, plus one hop of context: a diff says what moved,
	// the neighbours say who finds out.
	want := []string{"gno.land/p/a/v1", "gno.land/p/demo/ufmt", "gno.land/r/b/v0"}
	if got := modulesOf(g); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("nodes:\n got %v\nwant %v", got, want)
	}
	for _, n := range g.Nodes {
		if (n.Module == "gno.land/r/b/v0") != n.Changed {
			t.Fatalf("%s is marked changed=%v", n.Module, n.Changed)
		}
	}
}

func TestGraphRefusals(t *testing.T) {
	root := graphRepo(t)
	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		{"deps without a package", "need a package", []string{"graph", "-deps"}},
		{"both directions", "is the default", []string{"graph", "-deps", "-dependents", "p/a"}},
		{"changed with a package", "cannot also be given one", []string{"graph", "-changed", "HEAD", "p/a"}},
		{"two packages", "takes one package", []string{"graph", "p/a", "r/b"}},
		{"ambiguous package", "is ambiguous", []string{"graph", "p/a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			args := append([]string{tc.args[0], "-C", root}, tc.args[1:]...)
			err := Run(args, &out, &errw)
			if err == nil {
				t.Fatalf("expected an error mentioning %q, got none", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestCollapseVersionsKeyCollision is the regression test for a package
// silently disappearing from `gnopm graph -latest`.
//
// A versioned module keys on its unversioned base and a module with no version
// keys on its own path, so gno.land/p/demo/ufmt and gno.land/p/demo/ufmt/v0
// collided on one map key. The unversioned one was written first with rank 0,
// v0's `0 > 0` lost, and the versioned family was redirected onto an unrelated
// package: v0 vanished and every edge into it was redrawn onto the other node.
//
// Not exotic: it is what a migration looks like halfway through, the old
// unversioned path still on chain beside the new versioned one. No existing
// test could see it, because they all used workspaces where every path carried
// a version.
func TestCollapseVersionsKeyCollision(t *testing.T) {
	g := &Graph{
		Nodes: []GraphNode{
			{Module: "gno.land/p/demo/ufmt", External: true},
			{Module: "gno.land/p/demo/ufmt/v0"},
			{Module: "gno.land/r/x/a/v0"},
		},
		Edges: []GraphEdge{
			{From: "gno.land/r/x/a/v0", To: "gno.land/p/demo/ufmt/v0"},
			{From: "gno.land/r/x/a/v0", To: "gno.land/p/demo/ufmt"},
		},
	}
	collapseVersions(g)
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].Module < g.Nodes[j].Module })

	want := []string{"gno.land/p/demo/ufmt", "gno.land/p/demo/ufmt/v0", "gno.land/r/x/a/v0"}
	if got := modulesOf(g); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("a node was collapsed into an unrelated package:\n got %v\nwant %v", got, want)
	}
	wantEdges := []string{
		"gno.land/r/x/a/v0 -> gno.land/p/demo/ufmt",
		"gno.land/r/x/a/v0 -> gno.land/p/demo/ufmt/v0",
	}
	if got := edgesOf(g); strings.Join(got, ",") != strings.Join(wantEdges, ",") {
		t.Fatalf("an edge was redrawn onto the wrong node:\n got %v\nwant %v", got, wantEdges)
	}

	// And the collapse it is supposed to do still happens: two versions of the
	// same package become one node, with the edge on the newest.
	g2 := &Graph{
		Nodes: []GraphNode{
			{Module: "gno.land/p/demo/ufmt"},
			{Module: "gno.land/p/demo/ufmt/v0"},
			{Module: "gno.land/p/demo/ufmt/v1"},
		},
		Edges: []GraphEdge{{From: "gno.land/p/demo/ufmt/v1", To: "gno.land/p/demo/ufmt/v0"}},
	}
	collapseVersions(g2)
	want2 := []string{"gno.land/p/demo/ufmt", "gno.land/p/demo/ufmt/v1"}
	got2 := modulesOf(g2)
	sort.Strings(got2)
	if strings.Join(got2, ",") != strings.Join(want2, ",") {
		t.Fatalf("versions no longer collapse:\n got %v\nwant %v", got2, want2)
	}
	if len(g2.Edges) != 0 {
		t.Fatalf("v1 importing v0 should collapse to nothing, got %v", edgesOf(g2))
	}
}
