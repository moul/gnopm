package gnopm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// What actually went live, and writing it down.
//
// A green broadcast is not a deploy. A chain running code_submission_policy =
// "inert" stores the submission under inert_pkg:<path> and returns success
// without making it live; an approver enables it one or two blocks later, and
// until then the path answers "package not found" to every ordinary query. So
// an unattended publish that reports what it sent is reporting the wrong thing.
// It has to go back and read.
//
// And once it has read, it knows something the tree does not: where this
// version currently runs on a chain that is not the one its path names. That is
// the [[deployment]] row (deployment.go, moul/gno-meta#823), and this is the
// only place in gnopm that writes one, because it is the only place that knows
// the answer is true.

// confirmInterval is how often the poll asks again. Onyx turns a submission
// around in one to two blocks at ~3.5s, so three seconds sees every state
// change and the round below costs two queries however many packages went up.
const confirmInterval = 3 * time.Second

// confirmLive polls until every path is live or the deadline passes, and
// returns the set that is live.
//
// Deliberately not through Probe: that one caches, and its caches are built for
// the question "what was missing before this run". Here the whole point is that
// the answer has just changed.
func confirmLive(e *Env, c *Chain, paths []string, wait time.Duration) (map[string]bool, error) {
	live := map[string]bool{}
	if len(paths) == 0 {
		return live, nil
	}
	// wait <= 0 is one round and no waiting, which is what a run on its way out
	// asks for: it still has to know what is live before it writes the record.
	deadline := time.Now().Add(wait)
	for {
		var todo []string
		for _, p := range paths {
			if !live[p] {
				todo = append(todo, p)
			}
		}
		if len(todo) == 0 {
			return live, nil
		}
		found, err := livePathsNow(c, todo)
		if err != nil {
			return live, err
		}
		for p := range found {
			live[p] = true
		}
		done, total := len(live), len(paths)
		e.tracef("confirm  %d of %d live on %s\n", done, total, c.ID)
		if done == total || !time.Now().Before(deadline) {
			if done < total {
				e.logf("%s%s\n", e.label("wait"), e.warn(fmt.Sprintf(
					"%d of %d path(s) still not live on %s after %s: they were accepted, so they are parked and an approver has not enabled them yet",
					total-done, total, c.ID, wait)))
			}
			return live, nil
		}
		time.Sleep(confirmInterval)
	}
}

// livePathsNow asks which of these paths are live, right now, with nothing
// cached in either direction.
//
// ONE vm/qpaths per namespace, not one vm/qfile per path, and the difference is
// not a micro-optimisation. Everything one publish uploads sits under at most
// two prefixes, gno.land/p/<signer> and gno.land/r/<signer>, so a round costs
// two queries whether it is asking about three packages or three hundred.
//
// The per-path form was what this did first, and on a 46-package layer polled
// every two seconds it asked for roughly 1,400 queries a minute against a
// public endpoint whose measured ceiling is near 90 (see chain.go). It earned a
// 429 within minutes and then reported every path as absent, which is the one
// direction this must never be wrong in: it would have stopped a publish that
// was going perfectly well.
//
// The per-path read is kept as the fallback, for a chain with no vm/qpaths and
// for an answer at the page limit, which cannot be trusted to be complete.
func livePathsNow(c *Chain, paths []string) (map[string]bool, error) {
	out := map[string]bool{}
	byNS := map[string][]string{}
	var rest []string
	for _, p := range paths {
		if ns := namespacePrefix(p); ns != "" {
			byNS[ns] = append(byNS[ns], p)
			continue
		}
		rest = append(rest, p)
	}
	nss := make([]string, 0, len(byNS))
	for ns := range byNS {
		nss = append(nss, ns)
	}
	sort.Strings(nss)
	for _, ns := range nss {
		live, err := queryPaths(c, ns, bulkPathsLimit)
		if err != nil || len(live) >= bulkPathsLimit {
			// A truncated page might be missing entries, and a missing entry
			// reads as not-live, which here would stop the publish.
			rest = append(rest, byNS[ns]...)
			continue
		}
		set := make(map[string]bool, len(live))
		for _, q := range live {
			set[q] = true
		}
		for _, p := range byNS[ns] {
			if set[p] {
				out[p] = true
			}
		}
	}
	if len(rest) == 0 {
		return out, nil
	}
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	work := make(chan string)
	workers := probeConcurrency
	if len(rest) < workers {
		workers = len(rest)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				mu.Lock()
				stop := firstErr != nil
				mu.Unlock()
				if stop {
					continue
				}
				st, err := stateOf(c, p, nil, nil)
				mu.Lock()
				switch {
				case err != nil && firstErr == nil:
					firstErr = err
				case err == nil && st == StateLive:
					out[p] = true
				}
				mu.Unlock()
			}
		}()
	}
	for _, p := range rest {
		work <- p
	}
	close(work)
	wg.Wait()
	return out, firstErr
}

// recordDeployments writes the tree's side of the story: a [[deployment]] row
// for every package that went live under a path other than its own, and the
// removal of a chain's rows for every package that went live under its
// canonical one.
//
// The second half is #823's reading 2 and is the part that keeps the file
// honest over time: once mainnet holds the package the rehearsal is over, and a
// stale testnet URL left in the tree is worse than no URL at all. git history
// keeps the record if anybody wants it. -keep-deployments opts out.
//
// A package already live at its moved path gets a row too, even though this run
// did not send it. A publish of a workspace this size does not always finish in
// one go -- a layer whose approver is slow stops the run, by design -- and the
// row says where this version RUNS, not who put it there. Without this the
// record for everything an earlier attempt landed would be lost on the re-run
// that completes it.
//
// A version pinned to this repository's history is skipped, and that is not an
// oversight: its bytes come from the generated assembly under .gnopm, which is
// gitignored and rebuilt from a commit. There is no tracked file to write to,
// and writing into the directory that path USED to occupy would stamp a row
// onto whatever newer version lives there now.
func recordDeployments(e *Env, chainID, origin, revision string, plans []plan, live map[string]bool, keep bool) (int, error) {
	type edit struct {
		file string
		body string
		what string
	}
	var edits []edit
	for _, pl := range plans {
		if !live[pl.upload] {
			continue
		}
		if pl.upload == pl.pkg.Module && !publishes(pl) {
			// Dropping a record is destructive and adding one is not, so the
			// forget branch below only ever fires for a package THIS run put
			// at its canonical path. The record branch is happy to fill in
			// one an earlier run left behind; see recordDeployment's caller.
			continue
		}
		if pl.pinned {
			e.tracef("record   %s: pinned to history, no tracked gnomod.toml to write to\n", pl.pkg.Module)
			continue
		}
		file := filepath.Join(e.Root, filepath.FromSlash(pl.pkg.Dir), "gnomod.toml")
		b, err := os.ReadFile(file)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
		body := string(b)
		switch {
		case pl.upload != pl.pkg.Module:
			row := deployment{
				Chain:    chainID,
				Path:     pl.upload,
				URL:      deploymentURL(origin, pl.upload),
				Revision: revision,
				At:       nowUTC(),
			}
			next, err := recordDeployment(body, row)
			if err != nil {
				return 0, fmt.Errorf("%s/gnomod.toml: %w", pl.pkg.Dir, err)
			}
			if next != body {
				edits = append(edits, edit{file, next, "records " + pl.upload})
			}
		case !keep:
			next, dropped, err := forgetDeployment(body, chainID)
			if err != nil {
				return 0, fmt.Errorf("%s/gnomod.toml: %w", pl.pkg.Dir, err)
			}
			if dropped {
				edits = append(edits, edit{file, next, "drops the " + chainID + " row: the canonical path is live"})
			}
		}
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].file < edits[j].file })
	for _, ed := range edits {
		if err := os.WriteFile(ed.file, []byte(ed.body), 0o644); err != nil {
			return 0, err
		}
		e.tracef("record   %s %s\n", tildePath(ed.file), ed.what)
	}
	return len(edits), nil
}

// deployedPaths reads the tree's [[deployment]] rows for one chain.
//
// This is how a command that holds NO key finds out where a package went.
// verify -deployed cannot ask "may this signer use the namespace `moul`",
// because it has no signer; the rows the publish wrote are the mapping, and
// reading them back is what lets it compare the right bytes against the right
// path. Returns canonical -> path on that chain, for the packages that moved.
func deployedPaths(root string, pkgs []Package, chainID string) map[string]string {
	out := map[string]string{}
	for _, p := range pkgs {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p.Dir), "gnomod.toml"))
		if err != nil {
			continue
		}
		d, ok := deployedAt(string(b), chainID)
		if !ok || d.Path == "" || d.Path == p.Module {
			continue
		}
		out[p.Module] = d.Path
	}
	return out
}

// summarizeMoved is the one line the header spends on a path that moved, plus
// the examples a reader needs to believe it.
func summarizeMoved(e *Env, pm *pathMap, plans []plan, domain string) {
	if !pm.moved() {
		return
	}
	// Counted over the PLAN, not over the map. The map covers every module
	// this workspace could publish, which on a real workspace is a bigger
	// number than the run will touch, and a header line that quotes it is a
	// header line nobody can reconcile with the listing below it.
	moving := 0
	for _, pl := range plans {
		if publishes(pl) && pl.upload != pl.pkg.Module {
			moving++
		}
	}
	if moving == 0 {
		return
	}
	e.logf("%s%s\n", e.label("paths"), e.warn(fmt.Sprintf(
		"%d package(s) go up under %s, not the namespace in the tree: this signer may not deploy there",
		moving, pm.signer)))
	shown := 0
	for _, pl := range plans {
		if pl.upload == pl.pkg.Module || !publishes(pl) {
			continue
		}
		e.tracef("%s%s\n", e.label(""), e.dim(
			trimDomain(pl.pkg.Module, domain)+"  ->  "+trimDomain(pl.upload, domain)))
		shown++
		if shown == 3 && !e.Verbose {
			break
		}
	}
	e.logf("%s%s\n", e.label(""), e.dim(
		"imports between them are rewritten in the uploaded copy only; the tree is untouched"))

	// Everything the rewrite cannot reach, named rather than discovered on
	// chain. A path in a string literal is a txlink target, a realm-path
	// comparison or a prefix constant, and editing one would be a textual
	// change to program logic. See chainpath.go.
	outside := map[string]bool{}
	for _, pl := range plans {
		if !publishes(pl) {
			continue
		}
		for _, q := range pl.outside {
			outside[q] = true
		}
	}
	if len(outside) == 0 {
		return
	}
	var names []string
	for q := range outside {
		names = append(names, trimDomain(q, domain))
	}
	sort.Strings(names)
	e.logf("%s%s\n", e.label("note"), e.warn(fmt.Sprintf(
		"%d moved path(s) are also named OUTSIDE an import declaration and are left as written",
		len(names))))
	e.logf("%s%s\n", e.label(""), e.dim(
		"a string literal is a link target, a PkgPath() comparison or a prefix constant, not an import: "+
			"on this chain those name the canonical path, which is not where the package is"))
	for i, n := range names {
		if i == 5 && !e.Verbose {
			e.logf("%s%s\n", e.label(""), e.dim(fmt.Sprintf("... and %d more (-v lists them)", len(names)-5)))
			break
		}
		e.logf("%s%s\n", e.label(""), e.dim("  "+n))
	}
}

// uploadedPaths is every path this run sends, which is what the confirmation
// poll reads back.
func uploadedPaths(plans []plan) []string {
	var out []string
	for _, pl := range plans {
		if publishes(pl) {
			out = append(out, pl.upload)
		}
	}
	sort.Strings(out)
	return out
}

// liveURLs is a handful of real URLs for the report, so the last line of an
// unattended run is something a human can open.
func liveURLs(origin string, plans []plan, live map[string]bool, n int) []string {
	var out []string
	for _, pl := range plans {
		if len(out) == n {
			break
		}
		if !publishes(pl) || !live[pl.upload] {
			continue
		}
		if u := deploymentURL(origin, pl.upload); u != "" && strings.Contains(pl.upload, "/r/") {
			out = append(out, u)
		}
	}
	return out
}
