package gnopm

import (
	"flag"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
)

// Finding a package, which here means asking the chain rather than a registry.
//
// `npm search` and `cargo search` query a service somebody operates, which can
// rank, spam-filter, go down, and disagree with what you actually install.
// gno's registry is the chain, so search is a state query: vm/qpaths iterates
// the live key space under a prefix.
//
// That makes the result exact and boring in the good way. It is every package
// published under that prefix, in the order the store holds them, with no
// ranking to argue with and nothing between the answer and what an import of
// that path would resolve to.

// searchPathsLimit is the default page size.
//
// vm/qpaths takes ?limit= on the ABCI path, defaults to 1,000 and caps at
// 10,000 (gno.land/pkg/sdk/vm/handler.go, pathsLimit). Asking for the cap by
// default would make every search the most expensive query the node serves, for
// an answer nobody scrolls; asking for the node's own default matches what a
// reader of that file expects.
const searchPathsLimit = 1000

// Search lists published packages matching a term.
//
// Three shapes, because the chain natively understands two of them and people
// type the third:
//
//	gnopm search gno.land/p/moul   a path prefix, passed through
//	gnopm search @moul             a namespace: both p/ and r/, chain-side
//	gnopm search avl               a word: scanned client-side over one page
func Search(e *Env, term string, limit int, rpc, chainID string) error {
	term = strings.TrimSpace(term)
	if term == "" {
		return fmt.Errorf("nothing to search for: `gnopm search <term>`\n" +
			"  a path prefix (gno.land/p/moul), a namespace (@moul), or a word (avl)")
	}
	if limit <= 0 {
		limit = searchPathsLimit
	}

	// The domain decides which chain answers, so a bare word still needs one.
	// gno.land is the only domain that resolves today and is what every path
	// in a default workspace carries.
	domain := "gno.land"
	query, filter := term, ""
	switch {
	case strings.HasPrefix(term, "@"):
		// Passed through: the chain resolves a namespace across p/ and r/
		// itself, which client-side filtering could not do without two queries.
	case strings.Contains(term, "/"):
		if i := strings.Index(term, "/"); i > 0 && strings.Contains(term[:i], ".") {
			domain = term[:i]
		}
	default:
		// A word is not a prefix of anything, so ask for the whole domain and
		// scan. One query either way; the difference is how much comes back.
		query, filter = domain+"/", term
	}

	c, err := DiscoverChain(e, domain+"/p/x/v0", rpc, chainID)
	if err != nil {
		return err
	}
	paths, err := queryPaths(c, query, limit)
	if err != nil {
		return err
	}

	var hits []string
	for _, p := range paths {
		if filter != "" && !strings.Contains(p, filter) {
			continue
		}
		hits = append(hits, p)
	}
	sort.Strings(hits)

	if e.JSON {
		if hits == nil {
			hits = []string{}
		}
		return e.writeJSON(map[string]any{"chain": c.ID, "term": term, "paths": hits})
	}
	if len(hits) == 0 {
		// Nothing on stdout: an empty answer has to pipe as empty.
		e.logf("nothing on %s matches %q\n", c.ID, term)
		if filter != "" {
			e.logf("  scanned %d path(s). `gnopm search @<namespace>` searches one author,\n"+
				"  and -limit raises the page size (the chain caps it at 10000)\n", len(paths))
		}
		return nil
	}
	if e.Quiet {
		for _, p := range hits {
			e.printf("%s\n", p)
		}
		return nil
	}
	w := tabwriter.NewWriter(e.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PACKAGE\tKIND")
	for _, p := range hits {
		fmt.Fprintf(w, "%s\t%s\n", p, kindLabel(p))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// Saying the page was full is what stops somebody concluding a package does
	// not exist when it was simply past the cut.
	if len(paths) >= limit {
		e.logf("\nthe chain returned the full page of %d; there may be more. -limit raises it\n", limit)
	}
	e.logf("\n`gnopm get <package-path>` fetches one, `gnopm doc <package-path>` reads it\n")
	return nil
}

// kindLabel spells out what graph.go's kindOf returns, for a list humans scan.
//
// Built on kindOf rather than re-splitting the path, so there is one place that
// knows where the kind lives in a gno path. Anything other than p and r is
// reported as-is rather than guessed at: a chain may hold paths this tool has
// never heard of, and calling them "package" would be a small lie in a list
// people are reading to decide what to import.
func kindLabel(path string) string {
	switch k := kindOf(path); k {
	case "p":
		return "package"
	case "r":
		return "realm"
	case "":
		return "?"
	default:
		return k
	}
}

// queryPaths asks a chain for the live paths under a prefix.
//
// The limit rides on the ABCI path as a query string rather than in the data,
// which is how vm/qpaths reads it (pathsLimit cuts on "?"). Putting it in the
// data would be silently ignored and produce the default page instead.
func queryPaths(c *Chain, prefix string, limit int) ([]string, error) {
	raw, err := c.ABCIQuery(fmt.Sprintf("vm/qpaths?limit=%d", limit), prefix)
	if err != nil {
		return nil, fmt.Errorf("searching %s: %w", c.ID, err)
	}
	var out []string
	for _, p := range strings.Split(raw, "\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

func cmdSearch(e *Env, fs *flag.FlagSet, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("search takes one term: `gnopm search <term>`\n" +
			"  a path prefix (gno.land/p/moul), a namespace (@moul), or a word (avl)")
	}
	return Search(e, args[0], flagInt(fs, "limit"), flagString(fs, "rpc"), flagString(fs, "chainid"))
}
