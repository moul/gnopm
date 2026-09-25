package gnopm

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
}

// depBlock is one unsatisfied import and why it is unsatisfied. The two
// reasons need different words: an absent package outside this workspace is
// somebody's to deploy, while a parked one has already been sent and must not
// be sent again.
type depBlock struct {
	module string
	why    string
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

	// Only packages whose source is in the working tree can be published: a
	// version pinned to history exists to keep imports resolving, and
	// re-uploading one would publish code nobody is looking at. This is also
	// the set a dependency gets resolved against below.
	tree := map[string]Package{}
	var treeOrder []string
	for _, m := range sortedModules(lock) {
		if !m.Source.InTree() {
			continue
		}
		tree[m.Module] = Package{Dir: m.Source.Dir, Module: m.Module}
		treeOrder = append(treeOrder, m.Module)
	}

	selected := map[string]bool{}
	var matched []string
	for _, mod := range treeOrder {
		p := tree[mod]
		if pattern == "" || strings.Contains(p.Module, pattern) || strings.Contains(p.Dir, pattern) {
			selected[mod] = true
			matched = append(matched, mod)
		}
	}
	if len(matched) == 0 {
		return fmt.Errorf("no package in the working tree matches %q", pattern)
	}
	if pattern == "" {
		e.tracef("select   %d package(s) in the working tree\n", len(matched))
	} else {
		e.tracef("select   %d of %d package(s) in the working tree match %q\n",
			len(matched), len(treeOrder), pattern)
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
		imps, err := Imports(e.Root+"/"+tree[mod].Dir, domain)
		if err != nil {
			return err
		}
		out := imps[:0:0]
		for _, im := range imps {
			if im == mod {
				continue
			}
			out = append(out, im)
			if _, ours := tree[im]; ours && !selected[im] {
				selected[im] = true
				pulled[im] = true
				queue = append(queue, im)
			}
		}
		deps[mod] = out
	}

	var pkgs []Package
	for _, mod := range treeOrder {
		if selected[mod] {
			pkgs = append(pkgs, tree[mod])
		}
	}
	edges := 0
	for _, d := range deps {
		edges += len(d)
	}
	e.tracef("imports  read %d package(s), %d %s-prefixed import(s), %d pulled in as a dependency\n",
		len(deps), edges, domain, len(pulled))
	ordered := TopoOrder(pkgs, deps)

	inPlan := map[string]bool{}
	for _, p := range ordered {
		inPlan[p.Module] = true
	}

	// One batch, not one round trip per package. Everything the report needs
	// to know from the chain is known here, before anything is asked, so the
	// whole set goes up concurrently behind a bar, and whatever the cache
	// already knows never leaves this machine. On a workspace of any size this
	// is the difference between a few seconds and a minute of a terminal that
	// looks hung.
	var want []string
	for _, p := range ordered {
		want = append(want, p.Module)
		want = append(want, deps[p.Module]...)
	}
	e.tracef("chain    resolving %d path(s): every package and every import\n", len(want))
	bar := newProgress(e, "reading "+chain.ID)
	err = probe.Warm(want, bar.step)
	bar.stop()
	if err != nil {
		return err
	}

	haveInert := probe.CanPark()
	var plans []plan
	for _, p := range ordered {
		files, n, err := Payload(e.Root + "/" + p.Dir)
		if err != nil {
			return err
		}
		state, err := probe.State(p.Module)
		if err != nil {
			return err
		}
		pl := plan{pkg: p, state: state, bytes: n, files: files, dep: pulled[p.Module]}
		for _, d := range deps[p.Module] {
			ds, err := probe.State(d)
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
				pl.missing = append(pl.missing, depBlock{d, "not live, and not in this workspace"})
			}
		}
		plans = append(plans, pl)
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

	// The report goes to stderr, so that under -print stdout carries only the
	// script and under a real run it carries only the client's own output.
	// Nothing here is either.
	e.logf("chain    %s (%s)\n", chain.ID, chain.RPC)
	if chain.Host != "" {
		e.logf("         discovered from %s\n", chain.Host)
	}
	e.logf("key      %s\n", key)
	if gnokeyCmd != "gnokey" {
		e.logf("client   %s\n", gnokeyCmd)
	}
	if n := len(pulled); n > 0 {
		e.logf("deps     %d package(s) added: imported by what you named\n", n)
	}
	if !haveInert {
		e.logf("note     vm/qinertpaths unavailable: this chain cannot park a\n" +
			"         submission, so 'absent' really is absent\n")
	}
	// The cache line earns its place only when there is a cache and it did
	// something. Reporting it under -no-cache would contradict the flag, and
	// an always-on "0 answered from the cache" is the kind of line people stop
	// reading. A first run says "0 answered, 188 learned", which is worth
	// saying: it is the run that makes the next one fast.
	if file := probe.Cache().where(); file != "" {
		if _, hits, learned := probe.Cache().stats(); hits > 0 || learned > 0 {
			e.logf("cache    %d answered from %s, %d learned\n", hits, file, learned)
		}
	}

	// A line per package is 200 lines in this workspace, and all but a few of
	// them say "live", which is the one thing needing no decision. By default
	// report only what the run will act on or what stops it; -v is the full
	// listing, and it is what to reach for when the question is "why is that
	// package in the plan at all".
	todo, blocked, live := 0, 0, 0
	for _, pl := range plans {
		acts := pl.state == StateAbsent || pl.state == StateParked || len(pl.missing) > 0
		if e.Verbose || acts {
			marker := ""
			if pl.dep {
				marker = "  (dependency)"
			}
			e.logf("\n%-8s %s%s\n", pl.state, pl.pkg.Module, marker)
			e.logf("         %d bytes in %d file(s)", pl.bytes, len(pl.files))
			if len(pl.files) > 0 {
				e.logf(", largest %s at %d", pl.files[0].Name, pl.files[0].Size)
			}
			e.logf("\n")
		}
		for _, m := range pl.missing {
			e.logf("         BLOCKED by %s: %s\n", m.module, m.why)
		}
		switch {
		case len(pl.missing) > 0:
			blocked++
		case pl.state == StateAbsent:
			todo++
		case pl.state == StateParked:
			e.logf("         waiting on an approver; do not send it again\n")
		default:
			live++
		}
	}
	if live > 0 && !e.Verbose {
		e.logf("live     %d package(s) already on chain, nothing to do (-v lists them)\n", live)
	}

	for _, m := range mismatched {
		e.logf("\nPRIVATE  %s\n", m.module)
		e.logf("         %s\n", m.why())
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
		e.logf("\nnothing to publish\n")
		return nil
	}

	e.logf("\n%d package(s) to publish, in dependency order\n", todo)

	// The handoff is an output, not a hard-coded assumption about where the
	// key is. -o writes one document for one signature instead of N commands
	// for N signatures, which is strictly better whenever more than one
	// package is going up: it is atomic, so there is no half-deployed state
	// that neither the tree nor the chain describes.
	// Every message carries the creator as a field, so batching cannot start
	// without knowing which address will sign.
	creator, creatorErr := creatorFor(gnokeyCmdOf(fs), key, flagString(fs, "addr"))

	if out := flagString(fs, "o"); out != "" {
		// -o was asked for explicitly, so not knowing the address is an
		// error rather than a reason to quietly do something else.
		if creatorErr != nil {
			return fmt.Errorf("cannot tell which address will sign, and it is a field of every message: %w.\n"+
				"  Name it with -addr g1...", creatorErr)
		}
		return writeTxHandoff(e, fs, plans, deps, probe, key, creator, out)
	}

	// One signature per dependency LAYER is the default, because the
	// alternative is one per package and a workspace has hundreds. Packages
	// in a layer do not import each other, so batching them changes nothing
	// the chain can observe except how many times you type a passphrase.
	// -one-tx-per-package is the way back to a transaction each.
	if !flagBool(fs, "one-tx-per-package") {
		if creatorErr == nil {
			return writeTxHandoff(e, fs, plans, deps, probe, key, creator, defaultTxPath(e))
		}
		// Falling back rather than failing: a publish that works with more
		// prompts beats one that refuses over a name lookup. Say why, once.
		e.logf("note     one transaction per package: %v\n", creatorErr)
		e.logf("         -addr g1... batches them by dependency layer instead, far fewer prompts\n")
	}

	var cmds []publishCmd
	var totalFee int64
	for _, pl := range plans {
		if pl.state != StateAbsent || len(pl.missing) > 0 {
			continue
		}
		gas := GasFor(pl.bytes)
		totalFee += feeUgnot(gas)
		cmds = append(cmds, publishCmd{
			note: fmt.Sprintf("%s: %d bytes, %d gas (%d fixed + %d/byte), fee %s at %s ugnot/gas",
				pl.pkg.Module, pl.bytes, gas, gasFixed, gasPerByte, FeeFor(gas),
				strconv.FormatFloat(float64(feeRatioMicro)/1e6, 'g', -1, 64)),
			name: gnokeyCmd,
			groups: [][]string{
				{"maketx", "addpkg"},
				{"-pkgdir", e.Root + "/" + pl.pkg.Dir},
				{"-pkgpath", pl.pkg.Module},
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

	if flagBool(fs, "print") {
		e.printPublish(cmds, "generated by gnopm publish")
		if haveInert {
			e.printf("\n# This chain parks submissions: a green broadcast is NOT live.\n")
			e.printf("# Re-run `gnopm publish` to see whether an approver enabled them.\n")
		}
		return nil
	}

	e.logf("\nbroadcast %d package(s), %d ugnot in fees, one %s prompt each.\n", len(cmds), totalFee, gnokeyCmd)
	e.logf("          -print writes the commands out instead of running them.\n")
	if err := e.runPublish(cmds); err != nil {
		return err
	}
	if haveInert {
		e.logf("\nnote     this chain parks submissions: a green broadcast is NOT live.\n")
		e.logf("         Re-run `gnopm publish` to see whether an approver enabled them.\n")
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

// writeTxHandoff emits the whole deploy as one unsigned transaction document,
// or as a numbered set when it does not fit in one.
//
// gnopm still never signs. What changes is the shape of the handoff: a document
// the signer reads, signs once and broadcasts once, which also happens to be
// the shape a multisig ceremony needs, so a DAO-owned namespace gets the same
// path for free.
func writeTxHandoff(e *Env, fs *flag.FlagSet, plans []plan, deps map[string][]string, probe *Probe, key, creator, out string) error {
	chain := probe.Chain()

	// creator is an address, not a key name: it is a field of the message, so
	// gnopm has to know it before it can write anything. Resolving a keybase
	// name would mean reading gnokey's keybase, and gnopm is not a wallet.

	// One transaction per dependency LAYER, not one per workspace. Packages
	// in a layer are independent of each other, so the order they execute in
	// cannot matter and they can share a signature; a dependent waits for the
	// layer after its dependency, which is the only ordering the chain cares
	// about. See publishlayers.go for why the whole graph is not batched into
	// one transaction instead.
	layers, err := layerPlans(plans, deps)
	if err != nil {
		return err
	}
	gasOf := map[string]int64{}
	var docs []*TxDocument
	msgCount := 0
	for _, layer := range layers {
		var msgs []AddPackageMsg
		for _, pl := range layer {
			msg, err := AddPackageFor(e.Root+"/"+pl.pkg.Dir, pl.pkg.Module, creator, DepositFor(pl.bytes))
			if err != nil {
				return err
			}
			msgs = append(msgs, msg)
			gasOf[pl.pkg.Module] = GasFor(pl.bytes)
		}
		part, err := batchDocuments(msgs, func(m AddPackageMsg) int64 { return gasOf[m.Package.Path] })
		if err != nil {
			return err
		}
		docs = append(docs, part...)
		msgCount += len(msgs)
	}
	if len(docs) == 0 {
		e.logf("\nnothing to publish\n")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	paths, err := writeDocuments(out, docs)
	if err != nil {
		return err
	}

	e.logf("\nlayers   %d dependency layer(s) -> %d transaction(s) for %d package(s)\n",
		len(layers), len(docs), msgCount)
	e.logf("         a layer is packages that do not import each other, so one signature covers it\n")

	// Account number and sequence are covered by the signature, so they belong
	// in the command rather than in the document. Read them when the chain
	// will say, and be explicit about what that makes true.
	acct, acctErr := ReadAccount(chain, creator)

	e.logf("\ncreator  %s\n", creator)
	if len(docs) > 1 {
		e.logf("         they stay in dependency order, so sign and broadcast them in order\n")
	}
	for i, p := range paths {
		e.logf("\nwrote    %s (%d message(s))\n", p, len(docs[i].Msgs))
		for _, m := range docs[i].Msgs {
			e.logf("           %s\n", m.Package.Path)
		}
	}

	// The account number and sequence are covered by the signature, so a
	// document gnopm cannot number is a document only a human can finish.
	// Printing it with placeholders is useful; running it is not, so an
	// unreadable account forces the print form regardless of the flag.
	if acctErr != nil {
		e.printf("#!/bin/sh\n# generated by gnopm publish -o; fill in the two numbers, then run.\nset -e\n")
		for _, p := range paths {
			e.printf("\n%s sign \\\n", gnokeyCmdOf(fs))
			e.printf("  -tx-path %s \\\n", shellQuote(p))
			e.printf("  -chainid %s \\\n", chain.ID)
			e.printf("  -account-number <account-number> \\\n")
			e.printf("  -account-sequence <account-sequence> \\\n")
			e.printf("  %s\n", key)
			e.printf("%s broadcast -remote %s %s\n", gnokeyCmdOf(fs), chain.RPC, shellQuote(p))
		}
		e.logf("\nnote     could not read %s from the chain (%v), so the two numbers the\n", creator, acctErr)
		e.logf("         signature covers are unknown and this cannot be run for you.\n")
		e.logf("         gnokey query auth/accounts/%s -remote %s\n", creator, chain.RPC)
		return nil
	}

	var cmds []publishCmd
	for i, p := range paths {
		num := strconv.FormatUint(acct.Number, 10)
		seq := strconv.FormatUint(acct.Sequence+uint64(i), 10)
		what := fmt.Sprintf("%s: %d message(s), sequence %s", p, len(docs[i].Msgs), seq)
		cmds = append(cmds,
			publishCmd{
				note: "sign " + what,
				name: gnokeyCmdOf(fs),
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
				name:   gnokeyCmdOf(fs),
				groups: [][]string{{"broadcast"}, {"-remote", chain.RPC}, {p}},
			},
		)
	}

	if flagBool(fs, "print") {
		e.printPublish(cmds, "generated by gnopm publish -o")
	}
	// Saying the numbers without saying this would be worse than not saying
	// them: they are part of the signature, so anything else this account
	// signs first invalidates the document.
	e.logf("\nnote     account %d, sequence %d, read just now. The signature covers both,\n", acct.Number, acct.Sequence)
	e.logf("         so this document stops being valid the moment %s signs anything else.\n", key)
	if len(docs) > 1 {
		e.logf("         Each transaction takes the next sequence, which is why order matters.\n")
	}
	if flagBool(fs, "print") {
		return nil
	}
	// Everything in one document means one prompt, which is the reason -o
	// exists. Running it here is what makes that reason reachable without a
	// copy-paste that can go stale between the read and the paste.
	e.logf("\nsign     %d document(s) for %d package(s), one %s prompt each.\n",
		len(docs), msgCount, gnokeyCmdOf(fs))
	e.logf("         -print writes the commands out instead of running them.\n")
	return e.runPublish(cmds)
}

// gnokeyCmdOf is the client to name in an emitted command: a wrapper that adds
// a keybase or a remote signer takes the same arguments.
func gnokeyCmdOf(fs *flag.FlagSet) string {
	if c := flagString(fs, "gnokey-cmd"); c != "" {
		return c
	}
	return "gnokey"
}
