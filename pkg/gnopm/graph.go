package gnopm

import (
	"bytes"
	"flag"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Dependency graphs.
//
// gnopm already knows every edge: `why` and `tidy` are built on the same import
// graph, parsed from real import declarations rather than from any quoted path.
// So this is mostly presentation, and the interesting decisions are about what
// to leave out.
//
// Default output is DOT, not a rendered picture. A default that changes shape
// depending on whether graphviz happens to be installed makes
// `gnopm graph > g.dot` produce two different files on two machines, and CI is
// exactly where graphviz is absent. -svg asks for the picture and degrades to
// DOT with one line on stderr when it cannot have it.

// GraphNode is one module in the graph.
type GraphNode struct {
	Module string `json:"module"`
	// Kind is the path's second element: "p" for a package, "r" for a realm.
	Kind string `json:"kind"`
	// Source is "dir" for the working tree, "commit" for a version
	// materialized out of history, "" for a module this workspace only
	// imports.
	Source string `json:"source"`
	Dir    string `json:"dir,omitempty"`
	Commit string `json:"commit,omitempty"`
	// External marks a module that is imported from here but lives somewhere
	// else, so nothing in this workspace can bump or publish it.
	External bool `json:"external"`
	// Changed marks a module -changed selected, rather than one pulled in as
	// its neighbour.
	Changed bool `json:"changed,omitempty"`
}

// GraphEdge is one import.
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Graph is a whole projection, ready to render.
type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// GraphOptions is which projection was asked for.
type GraphOptions struct {
	// Root is the package the graph is about. "" is the whole workspace.
	Root string
	// Deps and Dependents narrow a rooted graph to one direction. Neither set
	// means both, which is the picture somebody standing in a package wants:
	// what it needs, and what needs it.
	Deps, Dependents bool
	// Latest keeps one node per package, its highest version, which is the
	// graph that goes in a README. An approximation on purpose: an edge onto
	// an older version is redrawn onto the latest one.
	Latest bool
	// Changed limits the graph to what changed since a ref, plus one hop of
	// context. "" is off; "auto" uses the ref verify already detects.
	Changed string
	// Internal drops modules this workspace does not contain.
	Internal bool
	// SVG renders with graphviz when it is on PATH.
	SVG bool
}

// BuildGraph assembles the projection.
func BuildGraph(e *Env, opts GraphOptions) (*Graph, error) {
	lock, err := readLock(e.Root)
	if err != nil {
		return nil, err
	}
	edges, err := importGraph(e.Root)
	if err != nil {
		return nil, err
	}

	nodes := map[string]*GraphNode{}
	for _, en := range lock.Modules {
		nodes[en.Module] = &GraphNode{
			Module: en.Module,
			Kind:   kindOf(en.Module),
			Source: en.Source.Variant(),
			Dir:    en.Source.Dir,
			Commit: en.Source.Commit,
		}
	}
	// Anything imported but not locked is somebody else's module. Keeping it
	// as a node is what makes the picture honest: an edge that leaves the
	// workspace is still a dependency, and hiding it makes a package look
	// self-contained when it is not.
	for from, tos := range edges {
		for _, m := range append([]string{from}, tos...) {
			if nodes[m] == nil {
				nodes[m] = &GraphNode{Module: m, Kind: kindOf(m), External: true}
			}
		}
	}

	adj, rev := map[string][]string{}, map[string][]string{}
	for from, tos := range edges {
		for _, to := range tos {
			adj[from] = append(adj[from], to)
			rev[to] = append(rev[to], from)
		}
	}

	keep := map[string]bool{}
	switch {
	case opts.Root != "":
		root, err := findLockedModule(lock, opts.Root)
		if err != nil {
			return nil, err
		}
		keep[root] = true
		if !opts.Dependents {
			reach(adj, root, keep)
		}
		if !opts.Deps {
			reach(rev, root, keep)
		}
	case opts.Changed != "":
		sel, err := changedModules(e.Root, opts.Changed, lock)
		if err != nil {
			return nil, err
		}
		if len(sel) == 0 {
			return &Graph{}, nil
		}
		for m := range sel {
			keep[m] = true
			nodes[m].Changed = true
			// One hop of context in both directions. A diff on its own says
			// what moved; the neighbours say who finds out.
			for _, n := range adj[m] {
				keep[n] = true
			}
			for _, n := range rev[m] {
				keep[n] = true
			}
		}
	default:
		for m := range nodes {
			keep[m] = true
		}
	}
	if opts.Internal {
		for m := range keep {
			if nodes[m].External {
				delete(keep, m)
			}
		}
	}

	g := &Graph{}
	for m := range keep {
		g.Nodes = append(g.Nodes, *nodes[m])
	}
	for from, tos := range edges {
		if !keep[from] {
			continue
		}
		for _, to := range tos {
			if keep[to] {
				g.Edges = append(g.Edges, GraphEdge{from, to})
			}
		}
	}
	if opts.Latest {
		collapseVersions(g)
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].Module < g.Nodes[j].Module })
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		return g.Edges[i].To < g.Edges[j].To
	})
	return g, nil
}

// reach marks everything transitively reachable from start.
func reach(adj map[string][]string, start string, seen map[string]bool) {
	queue := []string{start}
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		for _, n := range adj[m] {
			if !seen[n] {
				seen[n] = true
				queue = append(queue, n)
			}
		}
	}
}

// collapseVersions keeps the highest version of each package and re-points
// every edge at it, which is the graph a README wants: one node per package,
// and no history.
func collapseVersions(g *Graph) {
	// The two kinds of key live in separate namespaces, and they have to.
	//
	// A versioned module keys on its unversioned base, and a module with no
	// version keys on its own path, so gno.land/p/demo/ufmt and
	// gno.land/p/demo/ufmt/v0 collided on the same string. The unversioned one
	// was written first with rank 0, v0's `0 > 0` lost, and the whole versioned
	// family was then redirected onto an unrelated package: v0 vanished from
	// the picture and every edge into it was redrawn onto gno.land/p/demo/ufmt.
	//
	// That shape is not exotic here. It is what a migration looks like halfway
	// through: the old unversioned path still on chain, the new versioned one
	// beside it.
	best := map[string]string{}
	rank := map[string]int{}
	for _, n := range g.Nodes {
		if base, v, ok := splitVersion(n.Module); ok {
			if cur, seen := best["v\x00"+base]; !seen || v > rank[cur] {
				best["v\x00"+base] = n.Module
				rank[n.Module] = v
			}
			continue
		}
		best["m\x00"+n.Module] = n.Module
	}
	to := map[string]string{}
	for _, n := range g.Nodes {
		base, _, ok := splitVersion(n.Module)
		if !ok {
			to[n.Module] = n.Module
			continue
		}
		to[n.Module] = best["v\x00"+base]
	}
	var nodes []GraphNode
	kept := map[string]bool{}
	for _, n := range g.Nodes {
		if to[n.Module] == n.Module && !kept[n.Module] {
			kept[n.Module] = true
			nodes = append(nodes, n)
		}
	}
	var edges []GraphEdge
	seen := map[GraphEdge]bool{}
	for _, ed := range g.Edges {
		e := GraphEdge{to[ed.From], to[ed.To]}
		// A v1 importing v0 of the same package collapses to a self-edge,
		// which is noise rather than information.
		if e.From == e.To || seen[e] || !kept[e.From] || !kept[e.To] {
			continue
		}
		seen[e] = true
		edges = append(edges, e)
	}
	g.Nodes, g.Edges = nodes, edges
}

// changedModules maps the files a branch touched onto the packages that own
// them.
func changedModules(root, ref string, lock *Lock) (map[string]bool, error) {
	if ref == "auto" {
		ref = upstreamRef(root, "")
		if ref == "" {
			return nil, fmt.Errorf("no upstream ref detected: name one, `gnopm graph -changed origin/main`")
		}
	}
	out, err := git(root, "diff", "--name-only", ref+"...HEAD")
	if err != nil {
		return nil, fmt.Errorf("git diff against %s: %w", ref, err)
	}
	// Longest directory wins, so a nested package claims its own files rather
	// than losing them to the package above it.
	var dirs []LockEntry
	for _, en := range lock.Modules {
		if en.Source.InTree() && en.Source.Dir != "" {
			dirs = append(dirs, en)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].Source.Dir) > len(dirs[j].Source.Dir) })

	sel := map[string]bool{}
	for _, f := range strings.Split(out, "\n") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		for _, en := range dirs {
			if f == en.Source.Dir || strings.HasPrefix(f, en.Source.Dir+"/") {
				sel[en.Module] = true
				break
			}
		}
	}
	return sel, nil
}

func kindOf(module string) string {
	parts := strings.Split(module, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// DOT renders the graph as graphviz source.
func (g *Graph) DOT() string {
	var b strings.Builder
	b.WriteString("digraph gnopm {\n")
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("  node [fontname=\"Helvetica\", fontsize=10, shape=box, style=\"rounded,filled\", fillcolor=\"#ffffff\"];\n")
	b.WriteString("  edge [color=\"#888888\", arrowsize=0.7];\n")
	for _, n := range g.Nodes {
		var attrs []string
		attrs = append(attrs, fmt.Sprintf("label=%q", strings.TrimPrefix(n.Module, "gno.land/")))
		attrs = append(attrs, fmt.Sprintf("tooltip=%q", n.Module))
		switch {
		case n.External:
			// Nothing here can bump or publish it, so it has to look
			// different from everything that can be worked on.
			attrs = append(attrs, `style="rounded,dashed"`, `color="#aaaaaa"`, `fontcolor="#777777"`)
		case n.Source == "commit":
			// No directory left: it exists only because the lock remembers a
			// commit that still holds it.
			attrs = append(attrs, `style="rounded,filled,dashed"`, `fillcolor="#f4f0fa"`, `color="#8957e5"`)
		case n.Kind == "r":
			attrs = append(attrs, `fillcolor="#eaf5ea"`, `color="#2da44e"`)
		default:
			attrs = append(attrs, `fillcolor="#eef4fb"`, `color="#0969da"`)
		}
		if n.Changed {
			attrs = append(attrs, `penwidth=2.5`)
		}
		fmt.Fprintf(&b, "  %q [%s];\n", n.Module, strings.Join(attrs, ", "))
	}
	for _, e := range g.Edges {
		fmt.Fprintf(&b, "  %q -> %q;\n", e.From, e.To)
	}
	b.WriteString("}\n")
	return b.String()
}

// Render turns DOT into SVG with graphviz, or reports that it cannot.
func (g *Graph) Render() (svg string, err error) {
	dot, err := exec.LookPath("dot")
	if err != nil {
		return "", fmt.Errorf("graphviz is not on PATH")
	}
	cmd := exec.Command(dot, "-Tsvg")
	cmd.Stdin = strings.NewReader(g.DOT())
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("dot -Tsvg: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// GraphCmd builds and writes the graph.
func GraphCmd(e *Env, opts GraphOptions) error {
	g, err := BuildGraph(e, opts)
	if err != nil {
		return err
	}
	if e.JSON {
		if g.Nodes == nil {
			g.Nodes = []GraphNode{}
		}
		if g.Edges == nil {
			g.Edges = []GraphEdge{}
		}
		return e.writeJSON(g)
	}
	// The count goes to stderr, where it stays out of the pipe. `gnopm graph |
	// dot -Tsvg` has to receive DOT and nothing else.
	e.logf("%d node(s), %d edge(s)\n", len(g.Nodes), len(g.Edges))
	if opts.SVG {
		svg, err := g.Render()
		if err != nil {
			// Degrade rather than fail. Somebody piping this into a file on a
			// machine without graphviz gets something usable and one line
			// saying why it is not a picture.
			e.logf("%v, so this is DOT: pipe it through `dot -Tsvg` elsewhere\n", err)
		} else {
			e.printf("%s", svg)
			return nil
		}
	}
	e.printf("%s", g.DOT())
	return nil
}

func cmdGraph(e *Env, fs *flag.FlagSet, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("graph takes one package, got %d", len(args))
	}
	opts := GraphOptions{
		Deps:       flagBool(fs, "deps"),
		Dependents: flagBool(fs, "dependents"),
		Latest:     flagBool(fs, "latest"),
		Changed:    flagString(fs, "changed"),
		Internal:   flagBool(fs, "internal"),
		SVG:        flagBool(fs, "svg"),
	}
	if len(args) == 1 {
		opts.Root = args[0]
	}
	if opts.Root != "" && opts.Changed != "" {
		return fmt.Errorf("-changed selects its own packages, so it cannot also be given one")
	}
	if opts.Deps && opts.Dependents {
		return fmt.Errorf("-deps and -dependents together is the default: drop both")
	}
	if (opts.Deps || opts.Dependents) && opts.Root == "" {
		return fmt.Errorf("-deps and -dependents need a package to be about")
	}
	return GraphCmd(e, opts)
}
