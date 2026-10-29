package gnopm

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The [[deployment]] tables of gnomod.toml: where this version currently runs
// on a chain that is not the one its path names.
//
// This is the exact mirror of [source] (source.go), and the symmetry is the
// whole design rather than a coincidence:
//
//	[source]        written into the UPLOAD, never into the tree.
//	                A revision committed to the tree is stale one commit later.
//	[[deployment]]  written into the TREE, never into the upload.
//	                A testnet URL is useless to the chain that would store it,
//	                and it is the only thing a reader of the repository wants.
//
// Specified in moul/gno-meta#823. The row says which chain, the path the upload
// actually went to, the URL that renders it, the revision it was cut from and
// when:
//
//	[[deployment]]
//	  chain = "onyx-1"
//	  path = "gno.land/r/g1magld5ptgzaktqcexxq0pgq5lmdf0kz6re8hlv/zones/v0"
//	  url = "https://onyx.testnets.gno.land/r/g1magld5.../zones/v0"
//	  revision = "bf382ccb1a0f…"
//	  at = "2026-10-29T09:41:02Z"
//
// Three consumers, and the third is why the row exists at all rather than being
// recomputed:
//
//  1. A human reading the repository, who otherwise has the URL in a terminal
//     scrollback and nowhere else.
//  2. `gnopm publish`, which drops the rows for a version once mainnet holds
//     it: the rehearsal is over, and a stale testnet URL is worse than none.
//  3. `gnopm verify -deployed`, which cannot reconstruct the mapping on its
//     own. It holds no key, so it cannot ask "may this signer use `moul`"; the
//     rows ARE the mapping, and reading them is what lets it compare a package
//     against the right path on the right chain. See deployedPaths.
//
// gno's own parser ignores the table: File has no such field and
// tm2/pkg/toml's Unmarshal is non-strict (Strict is opt-in and gnomod does not
// opt in), so a tree carrying these rows parses exactly as it did before.
// `gno mod tidy` re-encodes from the struct and WOULD drop them, which is a
// real cost of putting this in gnomod.toml rather than beside it, and the
// reason gnopm edits the file as lines rather than through gno's encoder.

// deployment is one [[deployment]] row.
type deployment struct {
	Chain, Path, URL, Revision, At string
}

func (d deployment) zero() bool { return d == deployment{} }

// render writes the table in the two-space style the rest of gnomod.toml and
// [source] use.
func (d deployment) render() string {
	var b strings.Builder
	b.WriteString("[[deployment]]\n")
	fmt.Fprintf(&b, "  chain = %q\n", d.Chain)
	fmt.Fprintf(&b, "  path = %q\n", d.Path)
	if d.URL != "" {
		fmt.Fprintf(&b, "  url = %q\n", d.URL)
	}
	if d.Revision != "" {
		fmt.Fprintf(&b, "  revision = %q\n", d.Revision)
	}
	if d.At != "" {
		fmt.Fprintf(&b, "  at = %q\n", d.At)
	}
	return b.String()
}

var deploymentKey = regexp.MustCompile(`^\s*(chain|path|url|revision|at)\s*=\s*"([^"\\]*)"\s*(#.*)?$`)

// splitDeployments cuts every [[deployment]] table out of a gnomod.toml body
// and returns the rest, plus the rows it found, in file order.
//
// Line-based, like splitSource and normalizeGnomod, and for the same reason: a
// TOML round trip through gno's own encoder drops every key File does not
// declare, which is all of these. A value this cannot read is an error rather
// than a guess, because the alternative is silently losing a record of
// something that is live on a chain.
func splitDeployments(body string) (rest string, rows []deployment, err error) {
	var out []string
	var cur *deployment
	flush := func() {
		if cur != nil && !cur.zero() {
			rows = append(rows, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if tableHeader.MatchString(line) {
			flush()
			if t == "[[deployment]]" {
				cur = &deployment{}
				continue
			}
		}
		if cur == nil {
			out = append(out, line)
			continue
		}
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		m := deploymentKey.FindStringSubmatch(line)
		if m == nil {
			return "", nil, fmt.Errorf("cannot read %q in [[deployment]]: write it as key = \"value\"", t)
		}
		switch m[1] {
		case "chain":
			cur.Chain = m[2]
		case "path":
			cur.Path = m[2]
		case "url":
			cur.URL = m[2]
		case "revision":
			cur.Revision = m[2]
		case "at":
			cur.At = m[2]
		}
	}
	flush()
	return strings.Join(out, "\n"), rows, nil
}

// withoutDeployments is the uploaded form: every row gone, unconditionally and
// on every chain. The rule from #823 has no exceptions, because a row is a
// statement about a chain other than the one being written to.
func withoutDeployments(body string) (string, error) {
	rest, rows, err := splitDeployments(body)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return body, nil
	}
	return strings.TrimRight(rest, "\n") + "\n", nil
}

// withDeployments returns body carrying exactly rows, in the order given,
// appended after everything else. An empty slice removes the section.
func withDeployments(body string, rows []deployment) (string, error) {
	rest, _, err := splitDeployments(body)
	if err != nil {
		return "", err
	}
	rest = strings.TrimRight(rest, "\n")
	if len(rows) == 0 {
		if rest == "" {
			return "", nil
		}
		return rest + "\n", nil
	}
	var b strings.Builder
	b.WriteString(rest)
	for _, r := range rows {
		b.WriteString("\n\n")
		b.WriteString(strings.TrimRight(r.render(), "\n"))
	}
	b.WriteString("\n")
	return b.String(), nil
}

// recordDeployment folds one row into a body, replacing whatever that chain
// said before. One row per chain: a package is at one path on one chain, and
// keeping the previous answer would mean keeping a path that is no longer the
// one anybody should read.
//
// Idempotent, and that is not a nicety. `at` moves on every call, so a plain
// replace rewrites every file on every run: on moul/gno-contracts the second
// publish of a session touched 98 gnomod.toml to change nothing but a
// timestamp, which is 98 files of diff for a deploy that did not happen. A row
// whose chain, path, url and revision all still hold keeps the `at` it already
// had, because that is when the thing it describes actually happened.
func recordDeployment(body string, d deployment) (string, error) {
	_, rows, err := splitDeployments(body)
	if err != nil {
		return "", err
	}
	out := make([]deployment, 0, len(rows)+1)
	replaced := false
	for _, r := range rows {
		if r.Chain == d.Chain {
			if !replaced {
				next := d
				if r.Path == d.Path && r.URL == d.URL && r.Revision == d.Revision {
					next.At = r.At
				}
				out = append(out, next)
				replaced = true
			}
			continue
		}
		out = append(out, r)
	}
	if !replaced {
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Chain < out[j].Chain })
	return withDeployments(body, out)
}

// forgetDeployment drops the row for one chain, which is what a successful
// publish at the CANONICAL path means: the rehearsal is over, the real path
// holds the package, and a stale testnet URL left behind is worse than no URL
// at all (#823, reading 2). -keep-deployments opts out.
func forgetDeployment(body, chainID string) (string, bool, error) {
	_, rows, err := splitDeployments(body)
	if err != nil {
		return "", false, err
	}
	out := make([]deployment, 0, len(rows))
	dropped := false
	for _, r := range rows {
		if r.Chain == chainID {
			dropped = true
			continue
		}
		out = append(out, r)
	}
	if !dropped {
		return body, false, nil
	}
	s, err := withDeployments(body, out)
	return s, true, err
}

// deployedAt is the row for one chain, if the file carries one.
func deployedAt(body, chainID string) (deployment, bool) {
	_, rows, err := splitDeployments(body)
	if err != nil {
		return deployment{}, false
	}
	for _, r := range rows {
		if r.Chain == chainID {
			return r, true
		}
	}
	return deployment{}, false
}

// webOrigin is the gnoweb origin to build a URL from.
//
// The chain is normally discovered from the package path, and then Host is the
// origin that advertised the RPC endpoint. A run that passed -rpc and -chainid
// skipped discovery and has no Host, which is every unattended run against a
// testnet, so fall back to the RPC host with a leading "rpc." removed:
// rpc.onyx.testnets.gno.land -> onyx.testnets.gno.land, rpc.gno.land ->
// gno.land. "" when neither answers, and then the row simply carries no url.
func webOrigin(c *Chain, override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	if c.Host != "" {
		return strings.TrimRight(c.Host, "/")
	}
	host := c.RPC
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(strings.Trim(strings.SplitN(host, "/", 2)[0], "/"), ":443")
	rest, cut := strings.CutPrefix(host, "rpc.")
	if !cut || rest == "" {
		return ""
	}
	return "https://" + rest
}

// deploymentURL is where gnoweb renders an uploaded path: the origin plus
// everything after the domain element.
func deploymentURL(origin, path string) string {
	if origin == "" {
		return ""
	}
	_, rest, ok := strings.Cut(path, "/")
	if !ok {
		return ""
	}
	return origin + "/" + rest
}

// nowUTC is the timestamp a row carries, a seam so a test can pin it.
var nowUTC = func() string { return time.Now().UTC().Format(time.RFC3339) }
