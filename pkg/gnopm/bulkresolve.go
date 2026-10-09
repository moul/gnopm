package gnopm

import (
	"strings"
	"sync"
	"time"
)

// Asking "which of these paths are live" in one query per namespace.
//
// vm/qfile per path is one round trip per package, and a workspace resolves
// every package plus every gno.land import: 849 paths on moul/gno-contracts,
// of which the disk cache answered 322 and the chain was asked 527 times. That
// is the single largest block of reads a publish run makes, and it is also the
// shape a public endpoint rate-limits hardest.
//
// vm/qpaths returns every live path under a prefix in one response. Measured
// 2026-10-09 against rpc.gno.land: prefix gno.land/p/moul answered 123 paths in
// 1.18s, against 123 round trips for the same information. One call per
// namespace replaces the sweep, and it is also what the endpoint tolerates: a
// per-package sweep of the same catalogue has been seen answer 93 and then
// refuse the remaining 195, while the bulk call answered immediately from the
// same host (2026-09-28).
//
// Absent is still never written to disk. The bulk answer is this run's, for the
// same reason a negative cache is refused everywhere else here: absent is the
// state of the very version you are about to publish.

// Both are var rather than const so a test can shrink them: proving the
// truncation fallback with the real cap means faking ten thousand paths.
var (
	// bulkPathsLimit is the node's own cap on vm/qpaths (pathsLimit in
	// gno.land/pkg/sdk/vm/handler.go). Asking for exactly the cap is how a
	// truncated page is detectable: a full page might be missing entries, and
	// a missing entry reads as absent, which would propose republishing a
	// package that is already live.
	bulkPathsLimit = 10000

	// bulkNamespaceMin is how many unresolved paths in one namespace make the
	// bulk call worth making. A vm/qpaths over a whole namespace costs about
	// four vm/qfile reads, so below that it is a pessimisation, and a lone
	// import from some stranger's namespace is the common case.
	bulkNamespaceMin = 4
)

// bulkResolve answers what it can from one vm/qpaths per namespace and returns
// the paths still needing a read each.
//
// Every failure is a fallback, never an error: a chain with no vm/qpaths, a
// namespace too large to page in one call, a transport hiccup. The caller's
// per-path loop is still there and still correct, this only makes it shorter.
func (p *Probe) bulkResolve(cold []string, bump func(module string)) []string {
	byNS := map[string][]string{}
	var rest []string
	for _, m := range cold {
		ns := namespacePrefix(m)
		if ns == "" {
			rest = append(rest, m)
			continue
		}
		byNS[ns] = append(byNS[ns], m)
	}

	type result struct {
		ns   string
		live map[string]bool
	}
	var (
		mu      sync.Mutex
		results []result
		wg      sync.WaitGroup
		sem     = make(chan struct{}, probeConcurrency)
	)
	for ns, paths := range byNS {
		if len(paths) < bulkNamespaceMin {
			rest = append(rest, paths...)
			continue
		}
		wg.Add(1)
		go func(ns string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			live, ok := p.livePathsUnder(ns)
			mu.Lock()
			defer mu.Unlock()
			if !ok {
				rest = append(rest, byNS[ns]...)
				return
			}
			results = append(results, result{ns, live})
		}(ns)
	}
	wg.Wait()

	for _, r := range results {
		for _, m := range byNS[r.ns] {
			s := StateAbsent
			if r.live[m] {
				s = StateLive
				p.cached.learn(m)
			}
			p.mu.Lock()
			p.cache[m] = s
			p.mu.Unlock()
			p.trace(s, m, "vm/qpaths "+r.ns)
			bump(m)
		}
	}
	return rest
}

// livePathsUnder reads one namespace's live key space. ok is false whenever the
// answer cannot be trusted to be complete, which is the only way this can be
// wrong in the direction that matters.
func (p *Probe) livePathsUnder(ns string) (map[string]bool, bool) {
	start := time.Now()
	paths, err := queryPaths(p.chain, ns, bulkPathsLimit)
	if err != nil {
		return nil, false
	}
	if len(paths) >= bulkPathsLimit {
		return nil, false
	}
	// vm/qpaths matches a plain string prefix, so gno.land/p/moul also returns
	// gno.land/p/moulx. Dropping those is housekeeping, not a correctness
	// guard: the lookup below is an exact map hit, so a neighbour's path could
	// never answer for ours either way. It keeps the map to the namespace that
	// was asked for, which is what the -v line then reports.
	prefix := ns + "/"
	out := make(map[string]bool, len(paths))
	for _, q := range paths {
		if strings.HasPrefix(q, prefix) {
			out[q] = true
		}
	}
	if p.env != nil && p.env.Verbose {
		p.mu.Lock()
		p.env.tracef("chain    vm/qpaths %s: %d live path(s), in %s\n", ns, len(out), took(start))
		p.mu.Unlock()
	}
	return out, true
}

// namespacePrefix is the prefix a package path shares with its siblings:
// gno.land/p/moul/md/v0 -> gno.land/p/moul. It is namespaceOf's path form, and
// what vm/qpaths takes. "" means the path is too short to have siblings, which
// is not a package path anybody can publish.
func namespacePrefix(module string) string {
	parts := strings.Split(module, "/")
	if len(parts) < 4 {
		return ""
	}
	return strings.Join(parts[:3], "/")
}
