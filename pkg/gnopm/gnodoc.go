package gnopm

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Documentation as a first-class command, the way `go doc` is.
//
// Two things make this worth having rather than telling people to read the
// source.
//
// It documents a version that has no directory. `go doc` can only read what is
// in front of it, and the whole point of this tool is that a superseded version
// lives in gnomod.lock and is materialized on demand. `gnopm doc p/moul/md/v0`
// answers for a version nothing in the tree has, which is exactly the version
// somebody is asking about when they find an old import.
//
// And it needs no gno toolchain. gno source is Go syntax, so go/parser reads
// it: measured on 2026-09-25 against gno master, 1517 of 1517 .gno files under
// examples/ parse, and across examples, stdlibs and moul/gno-contracts, 3580 of
// 3590, where the only ten failures are gnovm's deliberately invalid fixtures
// (bad0.gno, empty.gno, invalid.gno and friends). Nothing here type-checks, and
// nothing here needs to: a signature and a doc comment are syntax.

// DocRecord is one documented declaration, and the value -f and -json see.
type DocRecord struct {
	// Kind is "package", "const", "var", "func", "type" or "method".
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Recv is the receiver type for a method, "" otherwise.
	Recv string `json:"recv,omitempty"`
	// Signature is the declaration as one line, with any body elided.
	Signature string `json:"signature,omitempty"`
	// Doc is the declaration's comment, unwrapped.
	Doc string `json:"doc,omitempty"`
}

// DocOptions is what doc was asked for.
type DocOptions struct {
	// Target is the package: a module path, a directory, or part of one. ""
	// means the package the working directory is in.
	Target string
	// Symbol narrows the answer to one declaration, like `go doc fmt.Println`.
	Symbol string
	// Unexported includes unexported declarations. -u, as `go doc` spells it.
	Unexported bool
}

// Doc renders a package's documentation.
func Doc(e *Env, opts DocOptions) error {
	lock, err := readLock(e.Root)
	if err != nil {
		return err
	}
	module, err := findLockedModule(lock, opts.Target)
	if err != nil {
		return err
	}
	entry, ok, _ := findModule(lock, module)
	if !ok {
		return fmt.Errorf("%s is not in %s", module, lockFile)
	}
	dir, err := sourceDirOf(e.Root, entry)
	if err != nil {
		return err
	}
	pkg, fset, err := parsePackageDoc(dir, module)
	if err != nil {
		return err
	}

	recs := docRecords(pkg, fset, opts.Unexported)
	if opts.Symbol != "" {
		recs = filterSymbol(recs, opts.Symbol)
		if len(recs) == 0 {
			return fmt.Errorf("%s has no symbol %q\n  `gnopm doc %s` lists what it does have", module, opts.Symbol, module)
		}
	}

	if e.JSON {
		return e.writeJSON(recs)
	}
	if e.Format != "" {
		vals := make([]any, len(recs))
		for i := range recs {
			vals[i] = recs[i]
		}
		return e.emit(vals...)
	}
	// Where the source came from is a diagnostic, not the answer, so it goes
	// to stderr and the documentation still pipes cleanly.
	if !entry.Source.InTree() {
		e.logf("%s has no directory: read from the assembly, pinned at %s\n",
			module, short(entry.Source.Commit))
	}
	renderDoc(e, module, pkg, recs, opts)
	return nil
}

// sourceDirOf resolves where a locked module's source actually is: the working
// tree for a { dir } entry, the assembly for a pinned one.
//
// The assembly is the interesting half and the reason this command exists. A
// version with no directory is still documented, as long as sync has
// materialized it, and saying so beats an empty answer.
func sourceDirOf(root string, e LockEntry) (string, error) {
	if e.Source.InTree() {
		return filepath.Join(root, filepath.FromSlash(e.Source.Dir)), nil
	}
	dir := filepath.Join(root, assemblyDir, filepath.FromSlash(e.Module))
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("%s is pinned to history and not materialized.\n  Run `gnopm sync` first", e.Module)
	}
	return dir, nil
}

// parsePackageDoc reads a package directory into go/doc.
//
// Test files are excluded, as `go doc` excludes them: they travel with the
// package and are charged for, but they document the tests rather than the
// package, and a _filetest.gno may declare a different package name entirely.
func parsePackageDoc(dir, importPath string) (*doc.Package, *token.FileSet, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	fset := token.NewFileSet()
	var files []*ast.File
	var names []string
	for _, en := range entries {
		n := en.Name()
		if en.IsDir() || !isProdGno(n) {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		src, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, nil, err
		}
		// Parsed from bytes under a .go name, because doc.NewFromFiles rejects
		// a file whose name does not end in .go. The label is never surfaced:
		// signatures are printed from the AST, and the errors below use the
		// real name. A gno package cannot contain a .go file, so nothing can
		// collide with the synthetic one.
		f, err := parser.ParseFile(fset, strings.TrimSuffix(n, ".gno")+".go", src, parser.ParseComments)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", n, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("%s has no gno source to document", importPath)
	}
	// AllDecls keeps unexported declarations in the tree so -u can show them;
	// filtering happens in docRecords, where the flag is known.
	p, err := doc.NewFromFiles(fset, files, importPath, doc.AllDecls)
	if err != nil {
		return nil, nil, err
	}
	return p, fset, nil
}

// docRecords flattens a documented package into the records every output shape
// is built from, so the text, the JSON and a -f template cannot disagree.
func docRecords(p *doc.Package, fset *token.FileSet, unexported bool) []DocRecord {
	out := []DocRecord{{Kind: "package", Name: p.Name, Doc: strings.TrimSpace(p.Doc)}}
	keep := func(name string) bool { return unexported || ast.IsExported(name) }

	for _, v := range p.Consts {
		for _, name := range v.Names {
			if keep(name) {
				out = append(out, DocRecord{Kind: "const", Name: name, Signature: declLine(fset, v.Decl), Doc: strings.TrimSpace(v.Doc)})
			}
		}
	}
	for _, v := range p.Vars {
		for _, name := range v.Names {
			if keep(name) {
				out = append(out, DocRecord{Kind: "var", Name: name, Signature: declLine(fset, v.Decl), Doc: strings.TrimSpace(v.Doc)})
			}
		}
	}
	for _, f := range p.Funcs {
		if keep(f.Name) {
			out = append(out, DocRecord{Kind: "func", Name: f.Name, Signature: funcLine(fset, f.Decl), Doc: strings.TrimSpace(f.Doc)})
		}
	}
	for _, t := range p.Types {
		if !keep(t.Name) {
			continue
		}
		out = append(out, DocRecord{Kind: "type", Name: t.Name, Signature: declLine(fset, t.Decl), Doc: strings.TrimSpace(t.Doc)})
		// go/doc files a const or var whose type is a named type in this
		// package under that TYPE, not under the package. Reading only
		// p.Consts therefore loses every such declaration, which for gno is
		// most of them: an enum-shaped `const Max Level = 6` is the normal
		// way to write one.
		for _, v := range t.Consts {
			for _, name := range v.Names {
				if keep(name) {
					out = append(out, DocRecord{Kind: "const", Name: name, Recv: t.Name, Signature: declLine(fset, v.Decl), Doc: strings.TrimSpace(v.Doc)})
				}
			}
		}
		for _, v := range t.Vars {
			for _, name := range v.Names {
				if keep(name) {
					out = append(out, DocRecord{Kind: "var", Name: name, Recv: t.Name, Signature: declLine(fset, v.Decl), Doc: strings.TrimSpace(v.Doc)})
				}
			}
		}
		// A constructor is listed under its type, as go doc does: it is how
		// you get one, so it belongs next to what it makes.
		for _, f := range t.Funcs {
			if keep(f.Name) {
				out = append(out, DocRecord{Kind: "func", Name: f.Name, Recv: t.Name, Signature: funcLine(fset, f.Decl), Doc: strings.TrimSpace(f.Doc)})
			}
		}
		for _, m := range t.Methods {
			if keep(m.Name) {
				out = append(out, DocRecord{Kind: "method", Name: m.Name, Recv: t.Name, Signature: funcLine(fset, m.Decl), Doc: strings.TrimSpace(m.Doc)})
			}
		}
	}
	return out
}

func filterSymbol(recs []DocRecord, symbol string) []DocRecord {
	// Type.Method, the way go doc takes it.
	recvWant, nameWant := "", symbol
	if i := strings.IndexByte(symbol, '.'); i >= 0 {
		recvWant, nameWant = symbol[:i], symbol[i+1:]
	}
	var out []DocRecord
	for _, r := range recs {
		if r.Kind == "package" {
			continue
		}
		if !strings.EqualFold(r.Name, nameWant) {
			continue
		}
		if recvWant != "" && !strings.EqualFold(r.Recv, recvWant) {
			continue
		}
		out = append(out, r)
		// Naming a type brings its methods with it, because "what can I do
		// with this" is the question being asked.
		if r.Kind == "type" && recvWant == "" {
			for _, m := range recs {
				if m.Recv == r.Name {
					out = append(out, m)
				}
			}
		}
	}
	return out
}

// funcLine renders a function as one line, with the body dropped.
func funcLine(fset *token.FileSet, fn *ast.FuncDecl) string {
	if fn == nil {
		return ""
	}
	stripped := *fn
	stripped.Doc = nil
	stripped.Body = nil
	return printNode(fset, &stripped)
}

// declLine renders a const, var or type declaration, collapsing a struct or
// interface body to `{ ... }`.
//
// Same choice go doc makes: the fields of a type are rarely what you came for,
// and a forty-field struct inlined into a listing buries everything after it.
// `gnopm doc <pkg> <Type>` prints the whole thing.
func declLine(fset *token.FileSet, decl *ast.GenDecl) string {
	if decl == nil {
		return ""
	}
	stripped := *decl
	stripped.Doc = nil
	stripped.Specs = nil
	for _, sp := range decl.Specs {
		ts, ok := sp.(*ast.TypeSpec)
		if !ok {
			stripped.Specs = append(stripped.Specs, sp)
			continue
		}
		clone := *ts
		clone.Doc, clone.Comment = nil, nil
		switch t := ts.Type.(type) {
		// Incomplete stays false: setting it makes go/printer emit its
		// "contains filtered or unexported fields" note, which is a line of
		// its own in a multi-line listing and pure noise collapsed onto one.
		case *ast.StructType:
			if t.Fields != nil && len(t.Fields.List) > 0 {
				clone.Type = &ast.StructType{Struct: t.Struct, Fields: &ast.FieldList{Opening: t.Fields.Opening, Closing: t.Fields.Closing}}
			}
		case *ast.InterfaceType:
			if t.Methods != nil && len(t.Methods.List) > 0 {
				clone.Type = &ast.InterfaceType{Interface: t.Interface, Methods: &ast.FieldList{Opening: t.Methods.Opening, Closing: t.Methods.Closing}}
			}
		}
		stripped.Specs = append(stripped.Specs, &clone)
	}
	return printNode(fset, &stripped)
}

func printNode(fset *token.FileSet, node any) string {
	var b bytes.Buffer
	if err := (&printer.Config{Mode: printer.UseSpaces, Tabwidth: 4}).Fprint(&b, fset, node); err != nil {
		return ""
	}
	// A collapsed body prints as `struct {\n}`, which the whitespace squeeze
	// leaves as `struct { }`. Say `struct{ ... }` instead, so it reads as
	// elided rather than as a type with no fields, which is a real and
	// different thing.
	s := strings.Join(strings.Fields(b.String()), " ")
	for _, kw := range []string{"struct", "interface"} {
		s = strings.ReplaceAll(s, kw+" { }", kw+"{ ... }")
		s = strings.ReplaceAll(s, kw+"{}", kw+"{ ... }")
	}
	// A parameter or result list written across several lines carries a
	// trailing comma, and squeezing it onto one leaves `( a int, b int, )`.
	// Put the punctuation back the way it is written on one line.
	s = strings.ReplaceAll(s, ", )", ")")
	s = strings.ReplaceAll(s, "( ", "(")
	s = strings.ReplaceAll(s, " )", ")")
	return s
}

func renderDoc(e *Env, module string, p *doc.Package, recs []DocRecord, opts DocOptions) {
	if opts.Symbol == "" {
		e.printf("package %s // import %q\n", p.Name, module)
		if d := strings.TrimSpace(p.Doc); d != "" {
			e.printf("\n%s\n", indent(d))
		}
		e.printf("\n")
	}
	last, first := "", true
	for _, r := range recs {
		if r.Kind == "package" {
			continue
		}
		// A blank line between kinds, so a listing has shape. Not before the
		// first one: `gnopm doc pkg Sym` would open on an empty line.
		if r.Kind != last {
			if !first {
				e.printf("\n")
			}
			last = r.Kind
		}
		first = false
		// Indent a declaration under the type it belongs to, but only in a
		// listing: asked for one symbol, it is the answer, not a sub-entry.
		prefix := ""
		if r.Recv != "" && opts.Symbol == "" {
			prefix = "    "
		}
		e.printf("%s%s\n", prefix, r.Signature)
		// Full doc when one symbol was asked for; the listing stays a listing.
		if opts.Symbol != "" && r.Doc != "" {
			e.printf("\n%s\n", indent(r.Doc))
		}
	}
	if opts.Symbol == "" && len(recs) == 1 {
		e.logf("\nnothing exported. `gnopm doc %s -u` includes unexported declarations\n", module)
	}
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ""
			continue
		}
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

func cmdDoc(e *Env, fs *flag.FlagSet, args []string) error {
	opts := DocOptions{Unexported: flagBool(fs, "u")}
	switch len(args) {
	case 0:
		// Standing in a package is an answer, the same way bump and why treat
		// it.
		pkg, err := packageAtCwd(e.Root, "doc")
		if err != nil {
			return err
		}
		opts.Target = pkg
	case 1:
		opts.Target = args[0]
	case 2:
		opts.Target, opts.Symbol = args[0], args[1]
	default:
		return fmt.Errorf("doc takes a package and optionally a symbol, got %d arguments", len(args))
	}
	return Doc(e, opts)
}
