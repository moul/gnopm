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
// Architecture decision 3 is the shape of this file: gnopm never signs. It
// reads the chain as much as it likes and then prints commands a human
// inspects and runs. So `gnopm publish` is safe to run at any moment, and
// `gnopm publish | sh` is the deliberate, explicit second step.
//
// Data (the commands) goes to Out and nothing else does, so the pipe receives
// a shell script. The report goes to Errw, where it stays visible to a human
// and invisible to the pipe.

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
	// gasPerByte is the top of the range measured over ten successful mainnet
	// add_package transactions above h160000 (2026-09-19): 1,014 to 1,781 gas
	// per uploaded byte, median 1,393. The spread is the package's own init()
	// work, which a byte count cannot see, so size from the top. gas_wanted is
	// a ceiling and a ceiling is not charged.
	gasPerByte = 1800

	// feeRatioMicro is the fee offered per unit of gas, in millionths of a
	// ugnot: 10_000 = 0.01 ugnot/gas.
	//
	// What the mempool enforces is the fee/gas_wanted RATIO, not the absolute
	// (EnsureSufficientMempoolFees), so raising the ceiling raises the required
	// fee and headroom is not free. The lowest ratio observed accepted on
	// gno.land mainnet is 0.001 ugnot/gas; this is ten times that, which
	// survives an upward drift for a rounding error. gas_fee is deducted in
	// full as offered and is NEVER refunded, unlike max_deposit, so
	// over-offering is a real cost rather than insurance.
	feeRatioMicro = 10_000

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

// GasFor sizes gas_wanted from the uploaded byte count.
func GasFor(bytes int) int64 { return int64(bytes) * gasPerByte }

// FeeFor sizes the gas fee from the ceiling it accompanies, never below 1ugnot
// because a zero fee is rejected outright.
func FeeFor(gasWanted int64) string {
	fee := gasWanted * feeRatioMicro / 1_000_000
	if fee < 1 {
		fee = 1
	}
	return strconv.FormatInt(fee, 10) + "ugnot"
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
				for _, p := range quotedPaths(rest, domain) {
					out = append(out, p)
				}
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
