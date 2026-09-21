package gnopm

import (
	"flag"
	"fmt"
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

	// The report goes to stderr so that `gnopm publish | sh` pipes only
	// commands. Nothing here is a command.
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

	todo, blocked := 0, 0
	for _, pl := range plans {
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
		}
	}

	if blocked > 0 {
		return fmt.Errorf("%d package(s) blocked: a dependency named above is not live, "+
			"and nothing in this workspace can publish it", blocked)
	}
	if todo == 0 {
		e.logf("\nnothing to publish\n")
		return nil
	}

	e.logf("\n%d package(s) to publish, in dependency order\n", todo)
	e.printf("#!/bin/sh\n# generated by gnopm publish; review before running.\n")
	e.printf("# gnopm never signs: these are commands for you to run.\nset -e\n")
	for _, pl := range plans {
		if pl.state != StateAbsent || len(pl.missing) > 0 {
			continue
		}
		gas := GasFor(pl.bytes)
		e.printf("\n# %s: %d bytes, %d gas at %d/byte, fee %s at %s ugnot/gas\n",
			pl.pkg.Module, pl.bytes, gas, gasPerByte, FeeFor(gas),
			strconv.FormatFloat(float64(feeRatioMicro)/1e6, 'g', -1, 64))
		e.printf("%s maketx addpkg \\\n", gnokeyCmd)
		e.printf("  -pkgdir %s \\\n", shellQuote(e.Root+"/"+pl.pkg.Dir))
		e.printf("  -pkgpath %s \\\n", shellQuote(pl.pkg.Module))
		e.printf("  -gas-wanted %d \\\n", gas)
		e.printf("  -gas-fee %s \\\n", FeeFor(gas))
		e.printf("  -max-deposit %dugnot \\\n", DepositFor(pl.bytes))
		e.printf("  -broadcast \\\n")
		e.printf("  -chainid %s \\\n", chain.ID)
		e.printf("  -remote %s \\\n", chain.RPC)
		e.printf("  %s\n", key)
	}
	if haveInert {
		e.printf("\n# This chain parks submissions: a green broadcast is NOT live.\n")
		e.printf("# Re-run `gnopm publish` to see whether an approver enabled them.\n")
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
