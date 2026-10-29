package gnopm

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Per-chain package paths: the same source, a different path on a chain that
// will not let this signer use the namespace.
//
// A gno package path IS its authorization. r/sys/names grants add_package under
// exactly two shapes (examples/gno.land/r/sys/names/verifier.gno, function
// `verifier`, read 2026-10-29): the namespace literal equals the signer's own
// address, or r/sys/users resolves the namespace to the signer's address and it
// is that user's CURRENT name. One address, no list, no delegation, and session
// accounts are barred from add_package outright.
//
// So gno.land/r/moul/zones/v0 can only ever be sent by the key that owns `moul`
// on that chain. Any other key, including an unattended agent key, cannot
// rehearse this workspace on a testnet under the paths it will carry on
// mainnet. Not because of a gnopm policy that could be relaxed: the chain
// refuses the transaction.
//
// The canonical path is what the package will be on mainnet, and it is what
// stays in the tree. On a chain where the signer cannot use that namespace the
// UPLOAD goes under the signer's own address path, which needs no registration
// anywhere, and gnomod.toml records where it landed (deployment.go).
//
// # What gno offers for this, and why none of it is the answer
//
//   - gnomod.toml HAS a replace directive. `[[replace]] old/new` is a real
//     field with a real resolver (gnovm/pkg/gnomod/file.go: Replace, Resolve,
//     AddReplace). It is refused on chain: checkGnomodConstraints opens with
//     `if gm.HasReplaces() { return ErrInvalidPackage("development packages are
//     not allowed") }` (gno.land/pkg/sdk/vm/keeper.go:657), and it runs on all
//     three paths that admit a package -- AddPackage's normal branch, its inert
//     branch, and EnablePackage. It is a local-toolchain device and nothing a
//     deploy can use.
//   - The chain has no path aliasing of any kind. checkNamespacePermission
//     matches the literal third element of the package path, and the VM
//     resolves an import by its literal string against the store. There is no
//     indirection to point at.
//   - gnosmoke rewrites the module line and nothing else (moul/gno-meta,
//     pkg/gnosmoke/run.go, manifest()): it discards the committed gnomod.toml
//     and synthesizes `module` plus `gno`. That is sufficient there because a
//     scenario is one self-contained package -- readMemFiles reads a single
//     directory and there is no dependency graph -- and it is NOT sufficient
//     here. An import names another package by path, so a package that moves
//     while its imports do not is a package whose dependencies do not exist on
//     the target chain.
//
// Which leaves rewriting the references, in the uploaded copy only, on the way
// out. That is what pathMap does.
//
// # What is rewritten, and what is deliberately not
//
// Rewritten: the `module` line of gnomod.toml, and every import declaration in
// every uploaded .gno file naming a package this run is moving.
//
// Not rewritten: a package path appearing anywhere else in the source. A path
// in a string literal is txlink target, a PrevRealm().PkgPath() comparison or a
// prefix constant, and gnopm has no way to tell which. Rewriting every quoted
// path in a file would be a textual edit to program logic; not rewriting it
// leaves a dead gnoweb link or an authorization check that does not match on
// the rehearsal chain. The second is recoverable and visible, the first is not,
// so the references outside the import declaration are REPORTED by name
// (outsideImports) and left alone.
//
// Note also that the module line is cosmetic as far as the chain is concerned:
// keeperOwnedGnomod sets `gm.Module = pkgPath` unconditionally, with "the
// deploy path wins over whatever the author declared" (gno.land/pkg/sdk/vm/
// pkghash.go:31). It is rewritten anyway, because the comparison in
// gnomodnorm.go reads it back and because a manifest naming a path the package
// is not at is a lie in a file a human reads. The IMPORTS are the half that
// actually decides whether the deploy works.

// pathMap says where each of this workspace's packages actually goes on one
// chain. Only the packages that MOVE have an entry, so a mainnet publish by the
// namespace owner carries a nil map and nothing downstream branches.
type pathMap struct {
	// to is canonical path -> upload path.
	to map[string]string
	// signer is the address every upload path is under, for the report.
	signer string
	// why is what the chain answered, per namespace, for -v.
	why map[string]string
}

// moved reports whether anything at all is going somewhere other than its
// canonical path.
func (m *pathMap) moved() bool { return m != nil && len(m.to) > 0 }

// Upload is where a canonical path goes on this chain. Safe on a nil map and
// on a path this workspace does not own, both of which answer "nowhere else".
func (m *pathMap) Upload(module string) string {
	if m == nil {
		return module
	}
	if u, ok := m.to[module]; ok {
		return u
	}
	return module
}

// count is how many packages move.
func (m *pathMap) count() int {
	if m == nil {
		return 0
	}
	return len(m.to)
}

// path mode values for -path.
const (
	pathAuto      = "auto"
	pathCanonical = "canonical"
	pathAddress   = "address"
)

// sysNamesDefault mirrors sysNamesPkgDefault in gno.land/pkg/sdk/vm/params.go.
// The chain's own value is read from params/vm:p:sysnames_pkgpath when it will
// say; this is what to ask when it will not.
const sysNamesDefault = "gno.land/r/sys/names"

// addressPath moves a package path into an address namespace:
// gno.land/r/moul/zones/v0 -> gno.land/r/g1magld.../zones/v0. Only the third
// element changes. ok is false for a path that has no namespace element to
// replace, or that is already under this address.
func addressPath(module, addr string) (string, bool) {
	parts := strings.Split(module, "/")
	if len(parts) < 4 {
		return "", false
	}
	if parts[1] != "p" && parts[1] != "r" {
		return "", false
	}
	if parts[2] == addr {
		return module, false
	}
	parts[2] = addr
	return strings.Join(parts, "/"), true
}

// chainPaths decides where each of modules goes on this chain.
//
// mode is one of the three -path values. "auto" asks the chain's own verifier,
// once per distinct namespace, and moves only what it says this signer may not
// use. Every other outcome -- a chain with no such realm, a transport failure,
// an answer this cannot read -- leaves the path alone and says why, because the
// two errors are not symmetric: publishing at the canonical path when the
// signer is not authorized wastes a transaction the chain refuses, and
// publishing at an address path when they were authorized puts the package
// somewhere nobody asked for, at a path that cannot be taken back.
func chainPaths(e *Env, c *Chain, mode, signer string, modules []string) (*pathMap, error) {
	m := &pathMap{to: map[string]string{}, signer: signer, why: map[string]string{}}
	switch mode {
	case pathCanonical:
		e.tracef("paths    -path canonical: every package goes to the path it carries in the tree\n")
		return m, nil
	case pathAuto, pathAddress, "":
	default:
		return nil, fmt.Errorf("-path %q: want one of %s, %s, %s", mode, pathAuto, pathCanonical, pathAddress)
	}
	if signer == "" || !bech32ish(signer) {
		// Without an address there is nothing to move TO, and guessing is not
		// an option: the address is what the namespace has to equal.
		e.tracef("paths    no signer address, so nothing can move: every package goes to its canonical path\n")
		return m, nil
	}

	nss := namespacesOf(modules)
	allowed := map[string]bool{}
	if mode == pathAddress {
		for _, ns := range nss {
			allowed[ns] = ns == signer
			m.why[ns] = "-path address"
		}
	} else {
		names := sysNamesPath(c)
		for _, ns := range nss {
			if ns == signer {
				allowed[ns] = true
				m.why[ns] = "the namespace is the signer's own address"
				continue
			}
			ok, err := c.authorizedForNamespace(names, signer, ns)
			if err != nil {
				// The safe direction. Say it out loud rather than in -v: a run
				// that silently assumed authorization and is wrong burns a
				// transaction per package.
				e.logf("%s%s\n", e.label("paths"), e.warn(fmt.Sprintf(
					"%s could not say whether %s may deploy under %q (%v): leaving the path as it is",
					c.ID, short(signer), ns, err)))
				allowed[ns] = true
				m.why[ns] = "unknown, left alone"
				continue
			}
			allowed[ns] = ok
			if ok {
				m.why[ns] = names + " says this signer owns it"
			} else {
				m.why[ns] = names + " says this signer may not deploy here"
			}
		}
	}

	for _, mod := range modules {
		ns := namespaceOf(mod)
		if allowed[ns] {
			continue
		}
		u, ok := addressPath(mod, signer)
		if !ok {
			continue
		}
		m.to[mod] = u
	}
	for _, ns := range nss {
		e.tracef("paths    %s: %s\n", ns, m.why[ns])
	}
	return m, nil
}

// namespacesOf is the distinct namespace element of each path, sorted, so the
// chain is asked once per namespace rather than once per package.
func namespacesOf(modules []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range modules {
		ns := namespaceOf(m)
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// sysNamesPath asks the chain which realm enforces namespaces, falling back to
// the default the chain itself falls back to.
func sysNamesPath(c *Chain) string {
	raw, err := c.ABCIQuery("params/vm:p:sysnames_pkgpath", "")
	if err != nil {
		return sysNamesDefault
	}
	v := strings.Trim(strings.TrimSpace(raw), `"`)
	if v == "" {
		return sysNamesDefault
	}
	return v
}

// authorizedForNamespace asks the chain's own verifier the question the chain
// will ask of the transaction.
//
// This is not a reimplementation of the rule and must never become one. The
// rule has two branches, one of which walks r/sys/users rename history
// (resolveCurrentName: a historical alias loses authority), and a local guess
// at it would be wrong in exactly the cases that matter. vm/qeval runs the
// realm's own function.
func (c *Chain) authorizedForNamespace(namesPkg, addr, ns string) (bool, error) {
	expr := fmt.Sprintf("%s.IsAuthorizedAddressForNamespace(%q,%q)", namesPkg, addr, ns)
	raw, err := c.ABCIQuery("vm/qeval", expr)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(raw) {
	case "(true bool)":
		return true, nil
	case "(false bool)":
		return false, nil
	}
	return false, fmt.Errorf("answered %q, which is not a bool", strings.TrimSpace(raw))
}

// moduleLineRe matches the module assignment of a gnomod.toml. Line-based, like
// everything else here that touches that file: gnopm has no TOML parser and
// does not want one, and re-encoding through gno's own would drop the
// [[deployment]] tables this repository keeps in the tree.
var moduleLineRe = regexp.MustCompile(`^(\s*module\s*=\s*)"[^"]*"(.*)$`)

// withModule rewrites the module line of a gnomod.toml body, or returns it
// unchanged when there is none to rewrite.
func withModule(body, module string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if m := moduleLineRe.FindStringSubmatch(l); m != nil {
			lines[i] = m[1] + `"` + module + `"` + m[2]
			return strings.Join(lines, "\n")
		}
	}
	return body
}

// rewriteImports replaces every import of a moved package with the path it
// moved to, inside the import declaration and nowhere else.
//
// It shares importRegion with Imports, rather than carrying a second copy of
// the grammar walk. Two copies would drift, and the consequence of drift here
// is an upload whose dependencies do not resolve on the chain it was sent to.
func rewriteImports(src, domain string, to func(string) string) string {
	lines, region := importRegion(src)
	changed := false
	for _, l := range region {
		for _, p := range quotedPaths(l.text, domain) {
			u := to(p)
			if u == p {
				continue
			}
			lines[l.i] = strings.ReplaceAll(lines[l.i], `"`+p+`"`, `"`+u+`"`)
			changed = true
		}
	}
	if !changed {
		return src
	}
	return strings.Join(lines, "\n")
}

// outsideImports names every moved path that this file mentions somewhere
// other than its import declaration: a txlink target, a realm-path comparison,
// a prefix constant. Those are program logic and are left exactly as written;
// this is what lets the report say so instead of leaving it to be discovered on
// chain. See the file comment for why rewriting them is not on the table.
func outsideImports(src, domain string, moved func(string) bool) []string {
	lines, region := importRegion(src)
	inRegion := map[int]bool{}
	for _, l := range region {
		inRegion[l.i] = true
	}
	seen := map[string]bool{}
	var out []string
	for i, l := range lines {
		if inRegion[i] {
			continue
		}
		for _, p := range quotedPaths(l, domain) {
			if !moved(p) || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// rewriteBody is the uploaded form of one file, given a map that moves paths.
// Anything not a .gno or the manifest travels byte for byte.
func (m *pathMap) rewriteBody(name, body, upload, domain string) string {
	switch {
	case name == "gnomod.toml":
		return withModule(body, upload)
	case strings.HasSuffix(name, ".gno"):
		return rewriteImports(body, domain, m.Upload)
	}
	return body
}
