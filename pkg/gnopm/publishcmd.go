package gnopm

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// plan is one package's publish plan.
type plan struct {
	pkg   Package
	state PackageState
	bytes int
	files []UploadFile
	// dep marks a package nothing matched the pattern: it is here because
	// something that did matched imports it.
	dep bool
	// missing are imports that are not live, that this run cannot fix, and
	// that therefore stop this package going up.
	missing []depBlock
	// republish marks a package already live that -republish will send again.
	// It is otherwise an ordinary entry: same payload, same gas, same batching.
	republish bool
	// changed names the files that differ from the chain's copy, set only for
	// a republish, so the report can say what the redeploy is actually for.
	changed []string
	// upload is the path this package actually goes to on this chain. It is
	// the canonical path, the one the tree carries and the one it will have on
	// mainnet, unless the signer may not deploy under that namespace: see
	// chainpath.go.
	upload string
	// bodies are the uploaded forms of the files that differ from the ones on
	// disk, by payload name. Two things write here and both write into the
	// copy only: the [source] table (source.go) and, when the path moves, the
	// module line and the intra-workspace imports (chainpath.go).
	bodies map[string]string
	// outside names the moved paths this package mentions somewhere other than
	// its import declaration, which are left exactly as written. Reported, not
	// rewritten: see chainpath.go.
	outside []string
	// pinned marks a package whose source left the working tree and is read
	// from the assembly, materialized from this repository's history. It
	// publishes exactly like one in the tree; the flag is for the report and
	// for the [source] section, which names the pinned commit rather than HEAD.
	pinned bool
	// skipped is why an already-live package the pattern selected will NOT be
	// republished. Reported rather than silent: under -republish, a package
	// quietly doing nothing is the failure mode worth spending a line on.
	skipped string
}

// depBlock is one unsatisfied import and why it is unsatisfied. The two
// reasons need different words: an absent package outside this workspace is
// somebody's to deploy, while a parked one has already been sent and must not
// be sent again.
type depBlock struct {
	module string
	why    string
}

// publishes reports whether a plan results in an addpkg message.
//
// Two reasons qualify and they are not interchangeable: absent means the path
// is empty, republish means the path is occupied by an older copy of the same
// package. Every consumer downstream treats them identically, which is the
// point, so the predicate is shared rather than restated: publishlayers.go
// carried its own copy of the absent-only form and silently dropped every
// republish from the batched document until this replaced it.
func publishes(pl plan) bool {
	if len(pl.missing) > 0 {
		return false
	}
	return pl.state == StateAbsent || pl.republish
}

func cmdPublish(e *Env, fs *flag.FlagSet, args []string) error {
	started := time.Now()
	// Deferred, so it is the last thing on stderr on every path out of here,
	// including the one where a missing dependency stops the plan. A timing
	// printed half way up the report is a timing for half the work.
	defer func() { e.tracef("\ntiming   %s end to end\n", took(started)) }()
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	e.tracef("lock     %s: %d module(s)\n", lockFile, len(lock.Modules))
	pattern := ""
	if len(args) > 0 {
		pattern = args[0]
	}
	rpc, chainID := flagString(fs, "rpc"), flagString(fs, "chainid")
	key := flagString(fs, "key")
	// The client is a command, not necessarily the binary called "gnokey": a
	// wrapper that adds a keybase, a remote signer, or an agent session key is
	// a normal thing to have, and hard-coding the name would force everyone
	// using one to post-process the script.
	gnokeyCmd := flagString(fs, "gnokey-cmd")
	if gnokeyCmd == "" {
		gnokeyCmd = "gnokey"
	}

	// Where each package's bytes are, for everything this repository may put
	// on a chain: the working tree AND the versions pinned to its own history
	// and materialized under .gnopm/. See publishsources.go for why those two
	// and not { chain }. This is also the set a dependency resolves against
	// below.
	src := publishSourcesOf(lock)
	e.tracef("sources  %d publishable module(s): %d in the working tree, %d pinned to history\n",
		len(src.order), len(src.tree), len(src.pin))

	matched := src.selectBy(pattern)
	selected := map[string]bool{}
	var pkgs []string
	for _, mod := range matched {
		// A pinned version is proved against the lock's hash before it is
		// allowed into the plan, so a pattern that names one with a stale
		// assembly says so here rather than uploading whatever is on disk.
		if _, ok := src.ours(e.Root, mod); !ok {
			e.logf("%s%s\n", e.label("skip"), e.warn(mod+": "+src.why[mod]))
			continue
		}
		selected[mod] = true
		pkgs = append(pkgs, mod)
	}
	matched = pkgs
	if len(matched) == 0 {
		if pattern == "" {
			return fmt.Errorf("this workspace has no publishable package")
		}
		return fmt.Errorf("no publishable package matches %q", pattern)
	}
	if pattern == "" {
		e.tracef("select   %d package(s) in the working tree\n", len(matched))
	} else {
		e.tracef("select   %d of %d publishable package(s) match %q\n",
			len(matched), len(src.order), pattern)
	}

	// Everything derived from "the package path" is derived from what the
	// pattern actually named, never from a dependency dragged in below: a
	// dependency can sit in another namespace, and guessing the key from it
	// would name the wrong signer for the package you asked about.
	primary := matched[0]
	probe, err := NewProbe(e, primary, rpc, chainID)
	if err != nil {
		return err
	}
	chain := probe.Chain()
	domain := primary
	if i := strings.IndexByte(domain, '/'); i >= 0 {
		domain = domain[:i]
	}

	// Detect, do not ask: a gno package path carries the namespace that owns
	// it, and a namespace is a user, so `gno.land/r/alice/home` is alice's to
	// publish and her key is overwhelmingly likely to be named `alice`. Guess
	// it, print the guess, and let -key override. Requiring the flag would be
	// friction paid on every invocation to restate what the path already says.
	if key == "" {
		key = namespaceOf(primary)
		if key == "" {
			return fmt.Errorf("cannot tell which key to name from %q: pass -key", primary)
		}
	}

	// Every message carries the creator as a field, so nothing can be batched
	// without knowing which address will sign. It is also what decides whether
	// the canonical paths are usable on this chain at all, which has to be
	// settled before a single path is read: the chain is asked about where the
	// packages are GOING, not about where they would go on mainnet.
	creator, creatorErr := creatorFor(gnokeyCmdOf(fs), key, flagString(fs, "addr"))

	// Follow the imports. Naming one package and being told its dependency is
	// missing, when that dependency is a directory in the same workspace, is
	// an answer that makes the user do the tool's job: work out the order,
	// then re-run publish once per package. A package cannot go up before
	// what it imports, so the set to publish is the import closure of what
	// was named, and only a dependency outside this workspace is genuinely
	// somebody else's problem.
	deps := map[string][]string{}
	pulled := map[string]bool{}
	queue := append([]string(nil), matched...)
	for len(queue) > 0 {
		mod := queue[0]
		queue = queue[1:]
		if _, done := deps[mod]; done {
			continue
		}
		imps, err := Imports(e.Root+"/"+src.pkg[mod].Dir, domain)
		if err != nil {
			return err
		}
		out := imps[:0:0]
		for _, im := range imps {
			if im == mod {
				continue
			}
			out = append(out, im)
			if _, ours := src.ours(e.Root, im); ours && !selected[im] {
				selected[im] = true
				pulled[im] = true
				queue = append(queue, im)
			}
		}
		deps[mod] = out
	}

	var ours []Package
	for _, mod := range src.order {
		if selected[mod] {
			ours = append(ours, src.pkg[mod])
		}
	}
	edges := 0
	for _, d := range deps {
		edges += len(d)
	}
	e.tracef("imports  read %d package(s), %d %s-prefixed import(s), %d pulled in as a dependency\n",
		len(deps), edges, domain, len(pulled))
	ordered := TopoOrder(ours, deps)

	inPlan := map[string]bool{}
	for _, p := range ordered {
		inPlan[p.Module] = true
	}

	// Where each package actually goes on THIS chain. Worked out over every
	// module this workspace may publish rather than over the plan alone,
	// because an import has to resolve to the same answer whoever asks: a
	// package whose path moves and a dependency of it that did not would be an
	// upload whose imports name nothing.
	//
	// Nothing below this line asks the chain about a canonical path. The chain
	// only ever knows about upload paths, so every State, every batch and every
	// republish comparison goes through paths.Upload.
	paths, err := chainPaths(e, chain, flagString(fs, "path"), creator, src.order)
	if err != nil {
		return err
	}
	if paths.moved() && flagBool(fs, "one-tx-per-package") {
		// gnokey reads the package directory itself under -pkgdir, so an
		// unbatched run uploads what is on disk and nothing can hand it the
		// rewritten imports. Refusing beats sending a package whose
		// dependencies do not exist on the chain it is going to.
		return fmt.Errorf("-one-tx-per-package cannot be used where the path moves: "+
			"%d package(s) would go up under %s, which rewrites their imports, and "+
			"`gnokey maketx addpkg -pkgdir` would upload the directory as it is on disk.\n"+
			"  Drop the flag to use transaction documents, or pass -path canonical to publish "+
			"at the paths the tree carries", paths.count(), short(creator))
	}

	// One batch, not one round trip per package. Everything the report needs
	// to know from the chain is known here, before anything is asked, so the
	// whole set goes up concurrently behind a bar, and whatever the cache
	// already knows never leaves this machine. On a workspace of any size this
	// is the difference between a few seconds and a minute of a terminal that
	// looks hung.
	var want []string
	for _, p := range ordered {
		want = append(want, paths.Upload(p.Module))
		for _, d := range deps[p.Module] {
			want = append(want, paths.Upload(d))
		}
	}
	e.tracef("chain    resolving %d path(s): every package and every import\n", len(want))
	bar := newProgress(e, "reading "+chain.ID)
	err = probe.Warm(want, bar.step)
	bar.stop()
	if err != nil {
		return err
	}

	haveInert := probe.CanPark()
	sources := newSourceResolver(e, !flagBool(fs, "source"))
	var plans []plan
	for _, p := range ordered {
		state, err := probe.State(paths.Upload(p.Module))
		if err != nil {
			return err
		}
		pl := plan{pkg: p, upload: paths.Upload(p.Module), state: state, dep: pulled[p.Module]}
		pin, pinned := src.pin[p.Module]
		pl.pinned = pinned
		// Sizing comes last, from the bytes that will actually be sent: gas and
		// the deposit ceiling are both derived from pl.bytes, so a rewrite made
		// after sizing is a transaction that asks for less than it is charged.
		if err := pl.prepareUpload(e.Root, sources, pinnedAt(pin, pinned), paths, domain); err != nil {
			return err
		}
		for _, d := range deps[p.Module] {
			ds, err := probe.State(paths.Upload(d))
			if err != nil {
				return err
			}
			switch {
			case ds == StateLive:
				// Already on chain: nothing to do and nothing to wait for.
			case ds == StateAbsent && inPlan[d]:
				// This same script publishes it, earlier, by construction of
				// the topological order.
			case ds == StateParked:
				// The bytes were accepted and are waiting for an approver.
				// Re-sending is not the fix and would duplicate the
				// submission, so this one waits.
				pl.missing = append(pl.missing, depBlock{d, "parked, not live until an approver enables it"})
			default:
				// Say which kind of elsewhere. A module gnopm has never heard
				// of is somebody's to deploy; one the lock DOES carry, that
				// this run still cannot publish, has a specific reason and a
				// command that fixes it, and printing the generic line for it
				// is what sent the last reader hunting for a version bump
				// nothing needed (moul/gno-meta#827).
				why := src.why[d]
				if why == "" {
					why = "not live, and not in this workspace"
				}
				pl.missing = append(pl.missing, depBlock{d, why})
			}
		}
		plans = append(plans, pl)
	}

	// -republish turns "already live" from the end of the story into a reason
	// to look closer. Only for packages the pattern actually named: a
	// dependency was pulled in to satisfy an import, and replacing one because
	// something else mentions it is not what anybody asked for.
	republish := flagBool(fs, "republish")
	includeDocs := flagBool(fs, "republish-docs")
	if includeDocs {
		republish = true
	}
	var scanner *republishScanner
	if republish {
		var targets []*plan
		for i := range plans {
			pl := &plans[i]
			if pl.state != StateLive || pl.dep || len(pl.missing) > 0 {
				continue
			}
			targets = append(targets, pl)
		}
		// Batched and cached rather than a serial loop, because at workspace
		// size the round trips are the entire cost: see republishscan.go.
		scanner = &republishScanner{
			chain:   chain,
			root:    e.Root,
			docs:    includeDocs,
			private: openPrivateCache(e.cacheDir(), chain.ID),
			source:  openSourceCache(e.cacheDir(), chain.ID),
		}
		start := time.Now()
		if err := scanner.scan(targets); err != nil {
			return err
		}
		e.tracef("chain    republish: compared %d live package(s) in %s\n", len(targets), took(start))
	}

	// The report goes to stderr, so that under -print stdout carries only the
	// script and under a real run it carries only the client's own output.
	// Nothing here is either.
	e.logf("%s%s  %s\n", e.label("chain"), e.bold(chain.ID), e.dim(chain.RPC))
	if chain.Host != "" {
		e.tracef("%s%s\n", e.label(""), e.dim("discovered from "+chain.Host))
	}
	if creator != "" {
		e.logf("%s%s  %s\n", e.label("key"), key, e.dim(creator))
	} else {
		e.logf("%s%s\n", e.label("key"), key)
	}
	if gnokeyCmd != "gnokey" {
		e.logf("%s%s\n", e.label("client"), gnokeyCmd)
	}
	summarizeMoved(e, paths, plans, domain)
	if !haveInert {
		e.tracef("%s%s\n", e.label("note"),
			e.dim("vm/qinertpaths unavailable: this chain cannot park a submission, so absent really is absent"))
	}
	// The cache line earns its place only when there is a cache and it did
	// something. Reporting it under -no-cache would contradict the flag, and
	// an always-on "0 answered from the cache" is the kind of line people stop
	// reading. A first run says "0 answered, 188 learned", which is worth
	// saying: it is the run that makes the next one fast.
	if file := probe.Cache().where(); file != "" {
		if _, hits, learned := probe.Cache().stats(); hits > 0 || learned > 0 {
			e.logf("%s%s\n", e.label("cache"),
				e.dim(fmt.Sprintf("%d answered, %d learned, in %s", hits, learned, tildePath(file))))
		}
	}
	// The republish scan's own two caches, reported separately because they
	// answer different questions and a reader deciding whether to pass
	// -no-cache deserves to see which one carried the run.
	if scanner != nil {
		if _, hits, learned := scanner.private.stats(); hits > 0 || learned > 0 {
			e.logf("%s%s\n", e.label("cache"),
				e.dim(fmt.Sprintf("private: %d answered, %d learned", hits, learned)))
		}
		if _, hits, learned := scanner.source.stats(); hits > 0 || learned > 0 {
			where := ""
			if f := scanner.source.where(); f != "" {
				where = ", in " + tildePath(f)
			}
			e.logf("%s%s\n", e.label("cache"),
				e.dim(fmt.Sprintf("chain source: %d answered, %d read in full%s", hits, learned, where)))
		}
	}
	if n := len(pulled); n > 0 {
		e.logf("%s%s\n", e.label("deps"),
			e.dim(fmt.Sprintf("%d package(s) added: imported by what you named", n)))
	}

	// Every package already on chain, which is the only set whose declared
	// `private` can be compared against anything. See privatecheck.go for why
	// a flag that is merely wrong, on a package publish will not touch, is
	// still worth stopping the run for.
	var livePkgs []Package
	for _, pl := range plans {
		if pl.state == StateLive {
			livePkgs = append(livePkgs, pl.pkg)
		}
	}
	mismatched, err := CheckPrivate(e, chain, e.Root, livePkgs)
	if err != nil {
		return err
	}

	out := flagString(fs, "o")
	if out != "" && creatorErr != nil {
		// -o was asked for explicitly, so not knowing the address is an
		// error rather than a reason to quietly do something else.
		return fmt.Errorf("cannot tell which address will sign, and it is a field of every message: %w.\n"+
			"  Name it with -addr g1...", creatorErr)
	}
	// One signature per dependency LAYER is the default, because the
	// alternative is one per package and a workspace has hundreds. Packages
	// in a layer do not import each other, so batching them changes nothing
	// the chain can observe except how many times you type a passphrase.
	// -one-tx-per-package is the way back to a transaction each.
	if out == "" && !flagBool(fs, "one-tx-per-package") && creatorErr == nil {
		out = defaultTxPath(e)
	}

	// The documents are built BEFORE the listing is printed, so that every
	// package row can say which transaction it lands in. That replaces a
	// per-transaction membership block that repeated every path a second time,
	// which on this workspace was twenty lines saying what eight already said.
	// Skipped when something is going to stop the run anyway: writing
	// documents for a plan that cannot be signed is work nobody reads.
	var hd *handoff
	if out != "" && blockedCount(plans) == 0 && len(mismatched) == 0 && todoCount(plans) > 0 {
		hd, err = buildHandoff(e, plans, deps, creator, out, int64(flagInt(fs, "min-gas")))
		if err != nil {
			return err
		}
	}

	// A line per package is 200 lines in this workspace, and all but a few of
	// them say "live", which is the one thing needing no decision. By default
	// report only what the run will act on or what stops it; -v is the full
	// listing, and it is what to reach for when the question is "why is that
	// package in the plan at all".
	todo, blocked, _ := reportPlans(e, plans, domain, hd.txOf(), hd.count())

	for _, m := range mismatched {
		e.logf("\n%s%s\n", e.label("PRIVATE"), e.bad(trimDomain(m.module, domain)))
		e.logf("%s%s\n", e.label(""), e.bad(m.why()))
	}

	if blocked > 0 {
		return fmt.Errorf("%d package(s) blocked: a dependency named above is not live, "+
			"and nothing in this workspace can publish it", blocked)
	}

	// Refused rather than warned. The flag cannot be corrected on chain, so the
	// only repair is to fix the repo or cut a new version, and a warning on a
	// command that then succeeds is a warning nobody comes back to.
	if n := len(mismatched); n > 0 {
		return fmt.Errorf("%d package(s) declare a private flag the chain does not hold: "+
			"the flag binds at the first publish and cannot be changed after, so fix the "+
			"gnomod.toml to match what is on chain, or publish a new version", n)
	}
	if todo == 0 {
		if republish {
			e.logf("\n%s%s\n", e.label("ok"), e.ok("nothing to publish or republish"))
		} else {
			e.logf("\n%s%s\n", e.label("ok"), e.ok("nothing to publish"))
		}
		return nil
	}

	if hd != nil {
		return emitHandoff(e, fs, hd, probe, key, creator, haveInert, plans, paths)
	}
	if out == "" && !flagBool(fs, "one-tx-per-package") {
		// Falling back rather than failing: a publish that works with more
		// prompts beats one that refuses over a name lookup. Say why, once.
		e.logf("%s%s\n", e.label("note"), e.warn("one transaction per package: "+creatorErr.Error()))
		e.logf("%s%s\n", e.label(""), e.dim("-addr g1... batches them by dependency layer instead, far fewer prompts"))
	}

	var cmds []publishCmd
	var totalFee int64
	for _, pl := range plans {
		if !publishes(pl) {
			continue
		}
		gas := gasAtLeast(pl.bytes, int64(flagInt(fs, "min-gas")))
		totalFee += feeUgnot(gas)
		cmds = append(cmds, publishCmd{
			note: fmt.Sprintf("%s: %d bytes, %d gas (%d fixed + %d/byte), fee %s at %s ugnot/gas",
				pl.pkg.Module, pl.bytes, gas, gasFixed, gasPerByte, FeeFor(gas),
				strconv.FormatFloat(float64(feeRatioMicro)/1e6, 'g', -1, 64)),
			short: "addpkg " + trimDomain(pl.pkg.Module, domain),
			name:  gnokeyCmd,
			groups: [][]string{
				{"maketx", "addpkg"},
				{"-pkgdir", e.Root + "/" + pl.pkg.Dir},
				{"-pkgpath", pl.upload},
				{"-gas-wanted", strconv.FormatInt(gas, 10)},
				{"-gas-fee", FeeFor(gas)},
				{"-max-deposit", strconv.FormatInt(DepositFor(pl.bytes), 10) + "ugnot"},
				{"-broadcast"},
				{"-chainid", chain.ID},
				{"-remote", chain.RPC},
				{key},
			},
		})
	}

	e.logf("\n%s%s\n", e.label("plan"), e.bold(fmt.Sprintf("%d package(s), one transaction each, %d ugnot in fees", len(cmds), totalFee)))

	if flagBool(fs, "print") {
		e.printPublish(cmds, "generated by gnopm publish")
		if haveInert {
			e.printf("\n# This chain parks submissions: a green broadcast is NOT live.\n")
			e.printf("# Re-run `gnopm publish` to see whether an approver enabled them.\n")
		}
		return nil
	}

	e.logf("%s%s\n", e.label("sign"), fmt.Sprintf("%d %s prompt(s); %s", len(cmds), gnokeyCmd, e.dim("-print writes the commands out instead")))
	if err := e.runPublish(cmds); err != nil {
		return err
	}
	return afterBroadcast(e, fs, chain, plans, paths, haveInert)
}

// afterBroadcast closes the loop a green broadcast leaves open.
//
// Two things happen here and neither of them is optional for an unattended
// run. The chain is read back, because a chain with the inert policy parks a
// submission and reports success, so what was accepted and what is live are
// different sets. Then what IS live is written into the tree, as the
// [[deployment]] rows that say where this version currently runs.
func afterBroadcast(e *Env, fs *flag.FlagSet, chain *Chain, plans []plan, pm *pathMap, haveInert bool) error {
	wait := flagDuration(fs, "wait")
	if wait <= 0 {
		if haveInert {
			e.logf("%s%s\n", e.label("note"), e.warn("this chain parks submissions: a green broadcast is NOT live. Re-run gnopm publish to see whether an approver enabled them."))
		}
		return nil
	}
	sent := uploadedPaths(plans)
	if haveInert {
		e.logf("%s%s\n", e.label("wait"), e.dim(fmt.Sprintf(
			"this chain parks submissions, so a green broadcast is not live: reading %d path(s) back, up to %s", len(sent), wait)))
	}
	start := time.Now()
	live, err := confirmLive(e, chain, sent, wait)
	if err != nil {
		return fmt.Errorf("reading %s back after the broadcast: %w", chain.ID, err)
	}
	e.logf("%s%s\n", e.label("live"), e.ok(fmt.Sprintf(
		"%d of %d path(s) live on %s, in %s", len(live), len(sent), chain.ID, took(start))))

	// The chain already told us about these, before anything was sent: they are
	// at a moved path and live, which on a workspace this size usually means an
	// earlier attempt landed them and stopped. They belong in the record.
	for _, pl := range plans {
		if pl.state == StateLive && pl.upload != pl.pkg.Module {
			live[pl.upload] = true
		}
	}

	origin := webOrigin(chain, flagString(fs, "web"))
	for _, u := range liveURLs(origin, plans, live, 3) {
		e.logf("%s%s\n", e.label(""), e.dim(u))
	}
	if !flagBool(fs, "record") {
		return nil
	}
	rev, _ := gitHead(e.Root)
	n, err := recordDeployments(e, chain.ID, origin, rev, plans, live, flagBool(fs, "keep-deployments"))
	if err != nil {
		return err
	}
	if n > 0 {
		e.logf("%s%s\n", e.label("record"), fmt.Sprintf(
			"%d gnomod.toml updated in the tree; commit them", n))
	}
	return nil
}

// shellQuote makes a value safe as one single-quoted shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// namespaceOf returns the namespace element of a gno package path, which is
// the third: domain / kind / namespace / name. An address namespace
// (gno.land/r/g1…) is returned as-is; it is a poor key name but a better
// starting point than nothing, and -key exists for it.
func namespaceOf(module string) string {
	parts := strings.Split(module, "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// handoff is the whole deploy as unsigned transaction documents: built, and
// written, before the report is printed so that every package row can name the
// transaction it lands in.
//
// gnopm still never signs. What changes is the shape of the handoff: a document
// the signer reads, signs once and broadcasts once, which also happens to be
// the shape a multisig ceremony needs, so a DAO-owned namespace gets the same
// path for free.
type handoff struct {
	docs     []*TxDocument
	paths    []string
	tx       map[string]int // module -> 1-based transaction it lands in
	layers   int
	msgCount int
}

// txOf and count are nil-safe, because the listing is printed whether or not a
// handoff was built (a blocked run builds none) and a caller should not have to
// branch to say so.
func (h *handoff) txOf() map[string]int {
	if h == nil {
		return nil
	}
	return h.tx
}

func (h *handoff) count() int {
	if h == nil {
		return 0
	}
	return len(h.docs)
}

// todoCount and blockedCount answer the two questions that decide whether
// building a handoff is worth doing at all, without printing anything. The
// listing counts the same things as it renders; these run before it.
func todoCount(plans []plan) int {
	n := 0
	for _, pl := range plans {
		if publishes(pl) {
			n++
		}
	}
	return n
}

func blockedCount(plans []plan) int {
	n := 0
	for _, pl := range plans {
		if len(pl.missing) > 0 {
			n++
		}
	}
	return n
}

// buildHandoff groups the plan into dependency layers, turns each into one or
// more transaction documents, and writes them.
//
// One transaction per dependency LAYER, not one per workspace. Packages in a
// layer are independent of each other, so the order they execute in cannot
// matter and they can share a signature; a dependent waits for the layer after
// its dependency, which is the only ordering the chain cares about. See
// publishlayers.go for why the whole graph is not batched into one transaction
// instead.
func buildHandoff(e *Env, plans []plan, deps map[string][]string, creator, out string, minGas int64) (*handoff, error) {
	layers, err := layerPlans(plans, deps)
	if err != nil {
		return nil, err
	}
	gasOf := map[string]int64{}
	// The chain speaks in upload paths and the report speaks in canonical
	// ones, so the index that joins them is built where both are in hand.
	canonical := map[string]string{}
	h := &handoff{tx: map[string]int{}, layers: len(layers)}
	for _, layer := range layers {
		var msgs []AddPackageMsg
		for _, pl := range layer {
			msg, err := pl.message(e.Root, creator)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, msg)
			gasOf[pl.upload] = gasAtLeast(pl.bytes, minGas)
			canonical[pl.upload] = pl.pkg.Module
		}
		part, err := batchDocuments(msgs, func(m AddPackageMsg) int64 { return gasOf[m.Package.Path] })
		if err != nil {
			return nil, err
		}
		h.docs = append(h.docs, part...)
		h.msgCount += len(msgs)
	}
	if len(h.docs) == 0 {
		return nil, nil
	}
	for i, d := range h.docs {
		for _, m := range d.Msgs {
			h.tx[canonical[m.Package.Path]] = i + 1
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return nil, err
	}
	h.paths, err = writeDocuments(out, h.docs)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// emitHandoff prints the transaction block and then signs and broadcasts, or
// prints the commands that would.
func emitHandoff(e *Env, fs *flag.FlagSet, h *handoff, probe *Probe, key, creator string, haveInert bool, plans []plan, pm *pathMap) error {
	chain := probe.Chain()

	// Account number and sequence are covered by the signature, so they belong
	// in the command rather than in the document. Read them when the chain
	// will say, and be explicit about what that makes true.
	acct, acctErr := ReadAccount(chain, creator)

	seq := ""
	if acctErr == nil {
		if len(h.docs) > 1 {
			seq = fmt.Sprintf(", sequence %d-%d", acct.Sequence, acct.Sequence+uint64(len(h.docs))-1)
		} else {
			seq = fmt.Sprintf(", sequence %d", acct.Sequence)
		}
	}
	e.logf("\n%s%s\n", e.label("plan"),
		e.bold(fmt.Sprintf("%d transaction(s) for %d package(s)%s", len(h.docs), h.msgCount, seq)))
	e.tracef("%s%s\n", e.label(""),
		e.dim(fmt.Sprintf("%d dependency layer(s); a layer is packages that do not import each other, so one signature covers it", h.layers)))
	for i, p := range h.paths {
		e.logf("%s%s  %s\n", e.label(""), tildePath(p), e.dim(fmt.Sprintf("%d msg", len(h.docs[i].Msgs))))
		for _, m := range h.docs[i].Msgs {
			e.tracef("%s%s\n", e.label(""), e.dim("  "+m.Package.Path))
		}
	}

	// The account number and sequence are covered by the signature, so a
	// document gnopm cannot number is a document only a human can finish.
	// Printing it with placeholders is useful; running it is not, so an
	// unreadable account forces the print form regardless of the flag.
	if acctErr != nil {
		e.printf("#!/bin/sh\n# generated by gnopm publish -o; fill in the two numbers, then run.\nset -e\n")
		for _, p := range h.paths {
			e.printf("\n%s sign \\\n", gnokeyCmdOf(fs))
			e.printf("  -tx-path %s \\\n", shellQuote(p))
			e.printf("  -chainid %s \\\n", chain.ID)
			e.printf("  -account-number <account-number> \\\n")
			e.printf("  -account-sequence <account-sequence> \\\n")
			e.printf("  %s\n", key)
			e.printf("%s broadcast -remote %s %s\n", gnokeyCmdOf(fs), chain.RPC, shellQuote(p))
		}
		e.logf("%s%s\n", e.label("note"),
			e.warn(fmt.Sprintf("could not read %s from the chain (%v): the two numbers the signature covers are unknown, so this cannot be run for you", creator, acctErr)))
		e.logf("%s%s\n", e.label(""),
			e.dim(fmt.Sprintf("gnokey query auth/accounts/%s -remote %s", creator, chain.RPC)))
		return nil
	}

	var cmds []publishCmd
	for i, p := range h.paths {
		num := strconv.FormatUint(acct.Number, 10)
		seq := strconv.FormatUint(acct.Sequence+uint64(i), 10)
		what := fmt.Sprintf("%s: %d message(s), sequence %s", p, len(h.docs[i].Msgs), seq)
		base := filepath.Base(p)
		cmds = append(cmds,
			publishCmd{
				note:  "sign " + what,
				short: "sign " + base + " " + e.dim("seq "+seq),
				name:  gnokeyCmdOf(fs),
				groups: [][]string{
					{"sign"},
					{"-tx-path", p},
					{"-chainid", chain.ID},
					{"-account-number", num},
					{"-account-sequence", seq},
					{key},
				},
			},
			publishCmd{
				note:   "broadcast " + p,
				short:  "broadcast " + base,
				name:   gnokeyCmdOf(fs),
				groups: [][]string{{"broadcast"}, {"-remote", chain.RPC}, {p}},
				then:   settleAfter(e, chain, h, i, haveInert, flagDuration(fs, "wait")),
			},
		)
	}

	if flagBool(fs, "print") {
		e.printPublish(cmds, "generated by gnopm publish -o")
	}
	// Saying the numbers without saying this would be worse than not saying
	// them: they are part of the signature, so anything else this account
	// signs first invalidates the document.
	e.logf("%s%s\n", e.label(""),
		e.dim(fmt.Sprintf("account %d: the signature covers the sequence, so %s signing anything else first invalidates %s",
			acct.Number, key, plural(len(h.docs), "this document", "these documents"))))
	if flagBool(fs, "print") {
		return nil
	}
	// Everything in one document means one prompt, which is the reason -o
	// exists. Running it here is what makes that reason reachable without a
	// copy-paste that can go stale between the read and the paste.
	e.logf("%s%s\n", e.label("sign"),
		fmt.Sprintf("%d %s prompt(s); %s", len(h.docs), gnokeyCmdOf(fs), e.dim("-print writes the commands out instead")))
	if err := e.runPublish(cmds); err != nil {
		return err
	}
	return afterBroadcast(e, fs, chain, plans, pm, haveInert)
}

// settleAfter is what a broadcast waits for before the NEXT document is
// signed, and it is not politeness.
//
// add_package TYPE-CHECKS a package's imports against the store, and a package
// the chain has PARKED is not in the store. The dependency layering puts an
// import in an earlier transaction, which is necessary and not sufficient:
// `gnokey broadcast` returns when the transaction is committed, and on a chain
// with the inert policy an approver needs another block or two after that
// before the packages it carried are importable. Measured on onyx-1 at ~3.5s a
// block, that is 3 to 7 seconds in which gnopm would otherwise sign and send a
// transaction whose imports do not exist, and be charged for the failure.
//
// Nothing to wait for on a chain that does not park: the packages are live the
// moment the transaction commits, so this returns nil and costs nothing. The
// last document has nothing after it either; afterBroadcast reads it back.
func settleAfter(e *Env, chain *Chain, h *handoff, i int, haveInert bool, wait time.Duration) func() error {
	if !haveInert || wait <= 0 || i >= len(h.docs)-1 {
		return nil
	}
	return func() error {
		var want []string
		for _, m := range h.docs[i].Msgs {
			want = append(want, m.Package.Path)
		}
		start := time.Now()
		e.logf("%s%s\n", e.label("wait"), e.dim(fmt.Sprintf(
			"tx%d parked %d package(s); the next transaction imports them, so waiting for an approver",
			i+1, len(want))))
		live, err := confirmLive(e, chain, want, wait)
		if err != nil {
			return fmt.Errorf("reading %s back after tx%d: %w", chain.ID, i+1, err)
		}
		if len(live) < len(want) {
			// Stopping here beats signing the next one. A package whose
			// imports are not live fails the type check, and the fee is
			// charged either way.
			var missing []string
			for _, p := range want {
				if !live[p] {
					missing = append(missing, p)
				}
			}
			sort.Strings(missing)
			return fmt.Errorf("tx%d went through but %d of its %d package(s) are still parked after %s: %s.\n"+
				"  The next transaction imports them and would fail its type check, so nothing more was sent.\n"+
				"  Re-run `gnopm publish` once an approver has enabled them",
				i+1, len(missing), len(want), wait, strings.Join(missing[:min(len(missing), 3)], ", "))
		}
		e.logf("%s%s\n", e.label("live"), e.ok(fmt.Sprintf("tx%d: %d package(s) live in %s", i+1, len(live), took(start))))
		return nil
	}
}

// gnokeyCmdOf is the client to name in an emitted command: a wrapper that adds
// a keybase or a remote signer takes the same arguments.
func gnokeyCmdOf(fs *flag.FlagSet) string {
	if c := flagString(fs, "gnokey-cmd"); c != "" {
		return c
	}
	return "gnokey"
}

// pinnedAt is the pin to stamp, or nil when the package is in the working tree
// and git itself is the answer.
func pinnedAt(en LockEntry, pinned bool) *Source {
	if !pinned {
		return nil
	}
	s := en.Source
	return &s
}

// prepareUpload works out the exact bytes this package uploads, which are not
// always the bytes on disk, and sizes the transaction from them.
//
// Three edits are made to the copy and none of them touches the tree:
//
//   - gnomod.toml loses its [[deployment]] rows. Unconditionally, on every
//     chain: a row is a statement about some OTHER chain, and useless to the
//     one being written to (deployment.go, moul/gno-meta#823).
//   - gnomod.toml gains a [source] table, when the repository is one that may
//     be named (source.go).
//   - When the path moves, gnomod.toml's module line and every intra-workspace
//     import in every .gno file are rewritten to the upload paths
//     (chainpath.go). Imports of anything outside this workspace are left
//     alone: they exist on the target chain under their real paths already.
//
// The byte count matters more than it looks. Gas and the storage-deposit
// ceiling are both sized from pl.bytes, so an edit made after sizing is a
// transaction that asks for less than it is charged. That is why this replaced
// a Payload() call followed by a separate stamping pass: there is one read of
// the directory, and the count is taken from what came out of it.
func (pl *plan) prepareUpload(root string, r *sourceResolver, pin *Source, pm *pathMap, domain string) error {
	dir := filepath.Join(root, filepath.FromSlash(pl.pkg.Dir))
	raw, err := payloadFiles(dir)
	if err != nil {
		return err
	}
	if pl.upload == "" {
		pl.upload = pl.pkg.Module
	}
	moved := pl.upload != pl.pkg.Module
	outside := map[string]bool{}
	files := make([]UploadFile, 0, len(raw))
	total := 0
	for _, f := range raw {
		b, err := os.ReadFile(f.Path)
		if err != nil {
			return err
		}
		disk := string(b)
		body := disk
		switch {
		case f.Name == "gnomod.toml":
			if body, err = withoutDeployments(body); err != nil {
				return fmt.Errorf("%s/gnomod.toml: %w", pl.pkg.Dir, err)
			}
			if moved {
				body = withModule(body, pl.upload)
			}
			sec, err := r.section(pl.pkg.Dir, body, pin)
			if err != nil {
				return err
			}
			if body, err = withSource(body, sec); err != nil {
				return err
			}
		case moved && strings.HasSuffix(f.Name, ".gno"):
			body = rewriteImports(body, domain, pm.Upload)
			for _, q := range outsideImports(disk, domain, func(path string) bool { return pm.Upload(path) != path }) {
				outside[q] = true
			}
		}
		if body != disk {
			if pl.bodies == nil {
				pl.bodies = map[string]string{}
			}
			pl.bodies[f.Name] = body
		}
		files = append(files, UploadFile{Name: f.Name, Size: len(body)})
		total += len(body) + len(f.Name)
	}
	for q := range outside {
		pl.outside = append(pl.outside, q)
	}
	sort.Strings(pl.outside)
	// Biggest first, because the report's job is to say what dominates the
	// cost. Same order Payload returns, and for the same reason.
	sort.Slice(files, func(i, j int) bool {
		if files[i].Size != files[j].Size {
			return files[i].Size > files[j].Size
		}
		return files[i].Name < files[j].Name
	})
	pl.files, pl.bytes = files, total
	return nil
}

// message is this package's MsgAddPackage: the directory as the toolchain
// reads it, sent to the path it actually goes to, with every file gnopm edited
// on the way out swapped in.
func (pl *plan) message(root, creator string) (AddPackageMsg, error) {
	msg, err := AddPackageFor(root+"/"+pl.pkg.Dir, pl.upload, creator, DepositFor(pl.bytes))
	if err != nil {
		return AddPackageMsg{}, err
	}
	for i := range msg.Package.Files {
		if b, ok := pl.bodies[msg.Package.Files[i].Name]; ok {
			msg.Package.Files[i].Body = b
		}
	}
	return msg, nil
}
