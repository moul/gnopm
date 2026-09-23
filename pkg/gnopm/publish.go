package gnopm

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Publish assistance: work out what is missing on a chain, in what order it
// has to go up, what it will cost, and emit the gnokey commands.
//
// gnopm never signs and never holds a key. That is the invariant, and it is
// unchanged: this file works out the commands, and gnokey is what signs them,
// prompting on your terminal for a passphrase gnopm never sees.
//
// What changed is who types them. `gnopm publish` used to print a script for a
// human to run, on the theory that the copy-paste was a review step. It was
// not: nobody reads 200 lines of generated addpkg, and the pipe that made it
// bearable (`gnopm publish | sh`) took stdin away from gnokey and broke the
// passphrase prompt outright. So publish runs them, in order, stopping at the
// first failure, and `-print` is there for the case where you genuinely do want
// to read the plan first.
//
// Data goes to Out and nothing else does: the script under -print, and the
// client's own output under a real run. The report goes to Errw.

// uploadedExtensions mirrors goodFileXtns in the gno toolchain
// (gnovm/pkg/gnolang/mempackage.go). `gnokey maketx addpkg` reads a package
// directory with MPUserAll, so test files and the README travel too, and they
// are what gas and the storage deposit are charged on. Sub-directories are
// skipped, except a `filetests/` the toolchain folds in.
var uploadedExtensions = []string{".gno", ".toml", ".md"}

// uploadedNames are whole filenames the toolchain accepts regardless of
// extension.
var uploadedNames = map[string]bool{
	"license": true, "license.txt": true,
	"licence": true, "licence.txt": true,
	"gno.mod": true,
}

const (
	// Gas for an add_package is NOT proportional to the payload. Measured over
	// 464 successful mainnet add_package transactions (2026-09-23, read back
	// from the tx-indexer with their exact uploaded bytes): gas per uploaded
	// byte runs from 989 to 16,435, a 16x spread, and a least-squares fit is
	//
	//	gas ~= 3,800,000 + 1,285 * bytes
	//
	// The constant term is what every add_package pays before the first byte of
	// source is charged for: parse, type-check, and run the package's init().
	// The residual above the fit is that init()'s own work, which a byte count
	// cannot see at all.
	//
	// A pure per-byte figure therefore cannot be a ceiling. The 1800/byte this
	// used to carry was measured over ten LARGE packages, where the constant
	// term is amortized away; replayed against the 464, it under-funds 173 of
	// them (37%), by up to 9.1x on a small one. That is the bug that made
	// `gnopm publish` die on a 4 KB package with "out of gas" after the big
	// ones in the same run had gone through.
	//
	// gasFixed and gasPerByte below are an envelope, not a fit: they cover every
	// one of the 447 non-system samples with room to spare, and all but four of
	// the 464. The four are r/gov/dao/*, r/sys/namereg and r/gnoland/wugnot,
	// whose init() builds a large tree at deploy time; nothing publishable from
	// a workspace looks like that, and sizing for them would tax every ordinary
	// package to insure against one nobody here will send.
	//
	// gas_wanted is a ceiling and the chain does not charge it, so the cost of
	// this headroom is only what the fee ratio below turns it into.
	gasFixed   = 12_000_000
	gasPerByte = 2600

	// maxBlockGas caps gas_wanted at the mainnet block limit (MaxGas in
	// /consensus_params, 3e9 on gnoland-1 at h271568). A transaction asking for
	// more than a block can hold is rejected outright, so clamping here turns an
	// impossible request into one the chain will at least attempt.
	maxBlockGas = 3_000_000_000

	// feeRatioMicro is the fee offered per unit of gas, in millionths of a
	// ugnot: 3_000 = 0.003 ugnot/gas.
	//
	// What the mempool enforces is the fee/gas_wanted RATIO, not the absolute
	// (EnsureSufficientMempoolFees compares fee/gas_wanted against the block gas
	// price), so raising the ceiling raises the required fee and headroom is not
	// free. gas_fee is deducted in full as offered and is NEVER refunded, unlike
	// max_deposit, so over-offering is a real cost rather than insurance.
	//
	// gnoland-1 reports 1ugnot per 1000 gas (auth/gasprice, h271568), i.e.
	// 0.001. This is three times that. It used to be ten times, which was
	// affordable only because the ceiling above it was too small to work: with a
	// ceiling that actually covers the fixed cost, 10x would have made a
	// 193-package publish cost ~82 GNOT in fees. At 3x it is ~25 GNOT, still
	// below what the broken-but-cheaper old pair quoted, and the margin is real:
	// the block gas price is dynamic (it rises when a block exceeds the target
	// gas ratio and decays to a floor otherwise), and a publish of this shape
	// spends 1-3% of a block, so it does not move the price it is paying.
	feeRatioMicro = 3_000

	// storagePerByte is what a realm write locks, in ugnot per byte.
	storagePerByte = 100
)

// PackageState is what a chain says about one package path.
type PackageState string

const (
	// StateLive means the package is deployed and callable.
	StateLive PackageState = "live"
	// StateParked means the bytes were accepted and are waiting for an
	// approver. A chain running code_submission_policy = "inert" stores a
	// submission under inert_pkg:<path> and returns success without making it
	// live, so a parked package answers "package not found" to every ordinary
	// query, exactly like an absent one. vm/qinertpaths is the only read that
	// tells them apart, and reporting "deployed" from a green broadcast is the
	// mistake this state exists to prevent.
	StateParked PackageState = "parked"
	// StateAbsent means the chain has never seen it.
	StateAbsent PackageState = "absent"
)

// UploadFile is one file of a MsgAddPackage payload.
type UploadFile struct {
	Name string
	Size int
}

// payloadFile is one file of the payload, with where it is on disk.
//
// Name is the flat base name the message carries, which is not the path for a
// filetest: the toolchain folds filetests/x_filetest.gno in as "x_filetest.gno".
type payloadFile struct {
	Name string
	Path string
	Size int
}

// payloadFiles lists exactly what `addpkg -pkgdir <dir>` uploads, in the order
// the toolchain reads them.
//
// Mirrors gno's ReadMemPackage with MPUserAll (gnovm/pkg/gnolang/mempackage.go,
// read against gno master on 2026-09-22): directory entries with an allowed
// extension or an allowed whole name, hidden files and subdirectories skipped,
// then the *_filetest.gno files from a filetests/ subdirectory appended.
//
// The filetests fold-in is easy to miss and was: skipping it under-counted the
// payload, so gas, fee and deposit were all sized from fewer bytes than the
// transaction actually carries, and a document built from this list would not
// have been the package gnokey would have sent.
func payloadFiles(dir string) ([]payloadFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []payloadFile
	add := func(name, path string) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		files = append(files, payloadFile{Name: name, Path: path, Size: int(info.Size())})
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !uploadable(name) {
			continue
		}
		if err := add(name, filepath.Join(dir, name)); err != nil {
			return nil, err
		}
	}
	ft, err := os.ReadDir(filepath.Join(dir, "filetests"))
	if err == nil {
		for _, e := range ft {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, "_filetest.gno") {
				continue
			}
			if err := add(name, filepath.Join(dir, "filetests", name)); err != nil {
				return nil, err
			}
		}
	}
	return files, nil
}

// Payload lists what `addpkg -pkgdir <dir>` would upload and the byte count
// gas and storage are charged on: file bodies plus their names, the same total
// the message carries. Biggest first, because the report's job is to say what
// dominates the cost.
func Payload(dir string) ([]UploadFile, int, error) {
	raw, err := payloadFiles(dir)
	if err != nil {
		return nil, 0, err
	}
	files := make([]UploadFile, 0, len(raw))
	total := 0
	for _, f := range raw {
		files = append(files, UploadFile{Name: f.Name, Size: f.Size})
		total += f.Size + len(f.Name)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Size != files[j].Size {
			return files[i].Size > files[j].Size
		}
		return files[i].Name < files[j].Name
	})
	return files, total, nil
}

func uploadable(name string) bool {
	if uploadedNames[strings.ToLower(name)] {
		return true
	}
	for _, x := range uploadedExtensions {
		if strings.HasSuffix(name, x) {
			return true
		}
	}
	return false
}

// GasFor sizes gas_wanted for one add_package: the fixed cost every deployment
// pays, plus the part that does scale with the payload, clamped to what a block
// can hold. See the gasFixed comment for where the two numbers come from and
// why a per-byte figure alone cannot be a ceiling.
func GasFor(bytes int) int64 {
	if bytes < 0 {
		bytes = 0
	}
	gas := gasFixed + int64(bytes)*gasPerByte
	if gas > maxBlockGas {
		return maxBlockGas
	}
	return gas
}

// FeeFor sizes the gas fee from the ceiling it accompanies, never below 1ugnot
// because a zero fee is rejected outright.
func FeeFor(gasWanted int64) string {
	return strconv.FormatInt(feeUgnot(gasWanted), 10) + "ugnot"
}

// feeUgnot is FeeFor as a number, for summing a plan. Split out rather than
// parsed back out of the string, because a total that disagrees with the
// commands it summarizes is worse than no total: it is the number someone
// checks their balance against before signing.
func feeUgnot(gasWanted int64) int64 {
	fee := gasWanted * feeRatioMicro / 1_000_000
	if fee < 1 {
		fee = 1
	}
	return fee
}

// DepositFor is a deliberate max_deposit: the source lock with headroom for
// the realm state the source bytes cannot account for, rounded up to whole
// GNOT. Omitting the flag is not opting out, it falls back to the chain's
// vm:p:default_deposit (100 GNOT of ceiling per message on gno.land). Unlike
// the gas fee this one is refundable and only the measured delta is locked, so
// headroom costs nothing.
func DepositFor(bytes int) int64 {
	const gnot = 1_000_000
	src := int64(bytes) * storagePerByte
	ceiling := (src*4/gnot + 1) * gnot
	if ceiling < 5*gnot {
		ceiling = 5 * gnot
	}
	return ceiling
}

// Imports returns the module paths a package's non-test files import, with the
// domain prefix kept. Test imports are excluded: they travel with the package
// but the VM never runs them, so a test-only dependency does not gate a
// deploy and requiring it on chain would block a valid upload.
func Imports(dir, domain string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".gno") || strings.HasSuffix(name, "_test.gno") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		for _, p := range importsIn(string(b), domain) {
			seen[p] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// importsIn pulls the domain-prefixed imports out of one source file, reading
// only the import declaration.
//
// This used to match a quoted domain-prefixed string ANYWHERE in the file, on
// the reasoning that gnopm does not parse gno and an import inside a comment
// costs one extra chain read. That reasoning was wrong about the cost. A path
// that is neither live nor in the workspace does not cost a read, it BLOCKS
// the package and everything importing it, with a message naming a dependency
// the package does not have:
//
//   - gno-contracts#201: a doc comment in p/moul/kit/ui named a realm path
//     that deversioning had removed, and kit/ui became unpublishable on every
//     chain.
//   - Measured again 2026-09-22 on the same repo, this time from ordinary
//     code and not a comment: strings.TrimPrefix(p, "gno.land/") made
//     "gno.land/" a dependency, and a prefix constant
//     "gno.land/r/moul/config/v" made that a dependency too. Two packages
//     blocked, and `publish` refused to emit a script for the other 53.
//
// So the scan is still textual, but it is bounded by the grammar instead of by
// the file: gno puts the import declaration between the package clause and the
// first other declaration, and nothing after that is an import. Comment lines
// inside the block are skipped, which is the #201 case.
func importsIn(src, domain string) []string {
	var (
		out       []string
		afterPkg  bool
		inBlock   bool
		inComment bool
	)
	for _, line := range strings.Split(src, "\n") {
		s := strings.TrimSpace(line)

		// A /* */ comment can hold anything, including the word import.
		if inComment {
			if i := strings.Index(s, "*/"); i >= 0 {
				inComment = false
				s = strings.TrimSpace(s[i+2:])
			} else {
				continue
			}
		}
		if i := strings.Index(s, "/*"); i >= 0 && !strings.Contains(s[:i], `"`) {
			if j := strings.Index(s[i:], "*/"); j < 0 {
				inComment = true
			}
			s = strings.TrimSpace(s[:i])
		}
		if s == "" || strings.HasPrefix(s, "//") {
			continue
		}

		if !afterPkg {
			// Everything before `package x` is the file's doc comment.
			if strings.HasPrefix(s, "package ") {
				afterPkg = true
			}
			continue
		}

		if inBlock {
			if strings.HasPrefix(s, ")") {
				inBlock = false
				// An import declaration can be followed by another one.
				continue
			}
			if p, ok := importOnLine(s, domain); ok {
				out = append(out, p)
			}
			// gofmt would not write `"path")`, but a block that never closes
			// would make the rest of the file look like imports again, which
			// is the bug this function exists to remove.
			if strings.HasSuffix(s, ")") {
				inBlock = false
			}
			continue
		}

		switch {
		case strings.HasPrefix(s, "import ("):
			inBlock = true
			// A whole block on one line, `import ( "a"; "b" )`, is legal and
			// closes where it opened.
			if rest := strings.TrimSpace(s[len("import ("):]); strings.HasSuffix(rest, ")") {
				inBlock = false
				out = append(out, quotedPaths(rest, domain)...)
			}
		case strings.HasPrefix(s, "import "):
			if p, ok := importOnLine(s, domain); ok {
				out = append(out, p)
			}
		default:
			// The first declaration that is not an import ends the header,
			// and with it anything that can be one.
			return out
		}
	}
	return out
}

// quotedPaths pulls every domain-prefixed quoted path out of one line, for the
// one-line import block where several can share it.
func quotedPaths(line, domain string) []string {
	var out []string
	for {
		p, ok := importOnLine(line, domain)
		if !ok {
			return out
		}
		out = append(out, p)
		i := strings.Index(line, `"`+p+`"`)
		line = line[i+len(p)+2:]
	}
}

// importOnLine pulls a domain-prefixed import out of one line of an import
// declaration.
func importOnLine(line, domain string) (string, bool) {
	needle := `"` + domain + `/`
	i := strings.Index(line, needle)
	if i < 0 {
		return "", false
	}
	rest := line[i+1:]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// TopoOrder orders packages so that every package comes after the ones it
// imports. Only edges inside the set count: a dependency outside it is either
// already on chain or reported missing, and either way it is not ours to
// order. Cycles cannot exist in gno imports, but a defensive pass appends
// anything left rather than looping.
func TopoOrder(pkgs []Package, deps map[string][]string) []Package {
	byModule := map[string]Package{}
	for _, p := range pkgs {
		byModule[p.Module] = p
	}
	var out []Package
	done := map[string]bool{}
	var visit func(string, map[string]bool)
	visit = func(m string, onPath map[string]bool) {
		if done[m] || onPath[m] {
			return
		}
		onPath[m] = true
		ds := append([]string{}, deps[m]...)
		sort.Strings(ds)
		for _, d := range ds {
			if _, ours := byModule[d]; ours {
				visit(d, onPath)
			}
		}
		delete(onPath, m)
		done[m] = true
		out = append(out, byModule[m])
	}
	modules := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		modules = append(modules, p.Module)
	}
	sort.Strings(modules)
	for _, m := range modules {
		visit(m, map[string]bool{})
	}
	return out
}
