package gnopm

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// docRepo is a package with something of every kind, bumped once, so the same
// symbol exists at two versions with two different signatures. That difference
// is the whole reason this command is not just `go doc`.
func docRepo(t *testing.T) string {
	t.Helper()
	root := newRepo(t)
	write(t, filepath.Join(root, "p/md/gnomod.toml"), "module = \"gno.land/p/md/v0\"\ngno = \"0.9\"\n")
	write(t, filepath.Join(root, "p/md/md.gno"), `// Package md renders markdown.
//
// Two paragraphs, so the doc is more than a sentence.
package md

// Level is a heading level.
type Level int

// Max is the deepest heading markdown has.
const Max Level = 6

// Sep separates blocks.
var Sep = "\n\n"

// Bold wraps s in asterisks.
func Bold(s string) string { return "**" + s + "**" }

func hidden() string { return "not exported" }

// Builder accumulates markdown.
type Builder struct {
	parts []string
	depth int
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder { return &Builder{} }

// Add appends a block.
func (b *Builder) Add(s string) *Builder { b.parts = append(b.parts, s); return b }

// String renders everything added so far.
func (b *Builder) String() string { return "" }

func (b *Builder) reset() {}
`)
	// A test file must not reach the documentation, and may even declare a
	// different package name.
	write(t, filepath.Join(root, "p/md/md_test.gno"), "package md\n\n// Helper is a test helper and must not be documented.\nfunc Helper() {}\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	return root
}

func TestDoc(t *testing.T) {
	root := docRepo(t)

	var out, errw bytes.Buffer
	if err := Run([]string{"doc", "-C", root, "gno.land/p/md/v0"}, &out, &errw); err != nil {
		t.Fatalf("doc: %v\n%s", err, errw.String())
	}
	got := out.String()
	for _, want := range []string{
		`package md // import "gno.land/p/md/v0"`,
		"Package md renders markdown.",
		"const Max Level = 6",
		`var Sep = "\n\n"`,
		"func Bold(s string) string",
		"type Builder struct{ ... }",
		"func NewBuilder() *Builder",
		"func (b *Builder) Add(s string) *Builder",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		"hidden", // unexported, and -u was not given
		"reset",  // an unexported method
		"Helper", // from a _test.gno, which is not the package's documentation
		"parts",  // a struct's fields are elided in a listing
		"is a test helper",
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("unexpected %q in:\n%s", unwanted, got)
		}
	}

	// -u opens the same listing up, which is what `go doc -u` does.
	out.Reset()
	if err := Run([]string{"doc", "-C", root, "gno.land/p/md/v0", "-u"}, &out, &errw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"func hidden() string", "func (b *Builder) reset()"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("-u did not show %q:\n%s", want, out.String())
		}
	}
}

// TestDocSymbol: naming one declaration prints its full doc, and naming a type
// brings its methods, because "what can I do with this" is the question.
func TestDocSymbol(t *testing.T) {
	root := docRepo(t)

	for _, tc := range []struct {
		name   string
		symbol string
		want   []string
		gone   []string
	}{
		{"a function", "Bold", []string{"func Bold(s string) string", "Bold wraps s in asterisks."}, []string{"NewBuilder"}},
		{"a type brings its methods", "Builder", []string{
			"type Builder struct{ ... }",
			"Builder accumulates markdown.",
			"func NewBuilder() *Builder",
			"func (b *Builder) Add(s string) *Builder",
		}, []string{"func Bold"}},
		{"one method", "Builder.Add", []string{"func (b *Builder) Add(s string) *Builder", "Add appends a block."}, []string{"NewBuilder"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			if err := Run([]string{"doc", "-C", root, "gno.land/p/md/v0", tc.symbol}, &out, &errw); err != nil {
				t.Fatalf("%v\n%s", err, errw.String())
			}
			// A symbol answer opens on the declaration, not on a blank line.
			if strings.HasPrefix(out.String(), "\n") {
				t.Fatalf("leading blank line:\n%q", out.String())
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Fatalf("missing %q in:\n%s", w, out.String())
				}
			}
			for _, w := range tc.gone {
				if strings.Contains(out.String(), w) {
					t.Fatalf("unexpected %q in:\n%s", w, out.String())
				}
			}
		})
	}

	var out, errw bytes.Buffer
	err := Run([]string{"doc", "-C", root, "gno.land/p/md/v0", "Nope"}, &out, &errw)
	if err == nil {
		t.Fatal("an unknown symbol was accepted")
	}
	if !strings.Contains(err.Error(), "no symbol") || !strings.Contains(err.Error(), "gnopm doc") {
		t.Fatalf("the error does not name the fix: %v", err)
	}
}

// TestDocReadsAVersionWithNoDirectory is the point of the command.
//
// `go doc` can only read what is in front of it. A superseded version has no
// directory at all: it lives in gnomod.lock and is materialized on demand, and
// it is exactly the version somebody is asking about when they meet an old
// import. Documenting it, and showing the signature that CHANGED, is something
// no other tool here can do.
func TestDocReadsAVersionWithNoDirectory(t *testing.T) {
	root := docRepo(t)
	mustRun(t, root, "bump", "p/md")
	// v1 changes Bold's signature. v0 keeps the old one, in history only.
	write(t, filepath.Join(root, "p/md/md.gno"), `// Package md renders markdown.
package md

// Bold wraps s in asterisks, and now reports whether it did anything.
func Bold(s string) (string, bool) { return "**" + s + "**", s != "" }
`)
	commit(t, root, "v1 changes Bold")
	mustRun(t, root, "sync")

	// The tree has v1 only.
	if _, err := readDirSafe(filepath.Join(root, "p/md")); err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	if err := Run([]string{"doc", "-C", root, "gno.land/p/md/v1"}, &out, &errw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "func Bold(s string) (string, bool)") {
		t.Fatalf("v1 signature wrong:\n%s", out.String())
	}

	// And v0, which has no directory anywhere in the tree.
	out.Reset()
	errw.Reset()
	if err := Run([]string{"doc", "-C", root, "gno.land/p/md/v0"}, &out, &errw); err != nil {
		t.Fatalf("doc on a pinned version: %v\n%s", err, errw.String())
	}
	if !strings.Contains(out.String(), "func Bold(s string) string") {
		t.Fatalf("v0's old signature is not documented:\n%s", out.String())
	}
	if strings.Contains(out.String(), "(string, bool)") {
		t.Fatalf("v0 was documented from v1's source:\n%s", out.String())
	}
	// Where it came from is a diagnostic, so it must not pollute the pipe.
	if !strings.Contains(errw.String(), "no directory") {
		t.Fatalf("it did not say the source came from the assembly: %q", errw.String())
	}

	// Without the assembly there is nothing to read, and the error says what
	// puts it back rather than pretending the version does not exist.
	mustRun(t, root, "clean")
	out.Reset()
	errw.Reset()
	err := Run([]string{"doc", "-C", root, "gno.land/p/md/v0"}, &out, &errw)
	if err == nil {
		t.Fatal("doc worked with no assembly")
	}
	if !strings.Contains(err.Error(), "gnopm sync") {
		t.Fatalf("the error does not name the fix: %v", err)
	}
}

// TestDocRecords: -json and -f see the same records the text is rendered from.
func TestDocRecords(t *testing.T) {
	root := docRepo(t)

	var out, errw bytes.Buffer
	if err := Run([]string{"doc", "-C", root, "gno.land/p/md/v0", "-json"}, &out, &errw); err != nil {
		t.Fatal(err)
	}
	var recs []DocRecord
	if err := json.Unmarshal(out.Bytes(), &recs); err != nil {
		t.Fatalf("-json is not records: %v\n%s", err, out.String())
	}
	kinds := map[string]int{}
	for _, r := range recs {
		kinds[r.Kind]++
	}
	for _, k := range []string{"package", "const", "var", "func", "type", "method"} {
		if kinds[k] == 0 {
			t.Fatalf("no %s record in %v", k, kinds)
		}
	}

	out.Reset()
	if err := Run([]string{"doc", "-C", root, "gno.land/p/md/v0", "-f", "{{.Kind}} {{.Name}}"}, &out, &errw); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"package md", "func Bold", "type Builder", "method Add"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("-f is missing %q:\n%s", want, out.String())
		}
	}
}

func readDirSafe(dir string) ([]string, error) {
	var out []string
	files, _, err := Payload(dir)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		out = append(out, f.Name)
	}
	return out, nil
}

// chainLockedRepo builds the shape moul/gnopm#71 was reported on: a one-module
// workspace whose lock entry came from a chain, with the bytes wherever the
// caller says. place is "vendor", "assembly", or "" for neither.
func chainLockedRepo(t *testing.T, place string) (root, module string) {
	t.Helper()
	root = newRepo(t)
	module = "gno.land/p/nt/ufmt/v0"
	src := `// Package ufmt is a tiny formatter.
package ufmt

// Sprintf formats according to a format specifier.
func Sprintf(format string, args ...string) string { return format }
`
	switch place {
	case "vendor":
		write(t, filepath.Join(vendorPathOf(root, module), "ufmt.gno"), src)
	case "assembly":
		write(t, filepath.Join(assemblyPathOf(root, module), "ufmt.gno"), src)
	}
	// The hash is real rather than a placeholder: doc does not check it, but a
	// fixture that could not satisfy verify would be a fixture that proves
	// nothing about the two agreeing, which is the whole point of #71.
	hash := "h1:0000000000000000000000000000000000000000000000000000000000000000"
	if place != "" {
		dir := vendorPathOf(root, module)
		if place == "assembly" {
			dir = assemblyPathOf(root, module)
		}
		h, err := hashDownloaded(dir)
		if err != nil {
			t.Fatal(err)
		}
		hash = h
	}
	write(t, filepath.Join(root, lockFile), "lock = 1\n\n[[module]]\nmodule = \""+module+"\"\n"+
		"source = { chain = \"gnoland-1\" }\nhash = \""+hash+"\"\n")
	return root, module
}

// TestDocReadsAChainModuleWhereverItsBytesAre pins moul/gnopm#71.
//
// doc branched on Source.InTree(), so every entry that was not the working
// tree went down the pinned-to-history path and was looked for in the assembly
// alone. A vendored { chain } module is deliberately absent from there, so doc
// called it unmaterialized and told you to run `gnopm sync`, which cannot
// materialize what is already vendored. verify, on the same lock in the same
// workspace, found it.
//
// Both chain placements are asserted, because the one that worked did so by
// coincidence: the assembly path happened to be the one the pinned branch
// built, not because anything knew about chain sources.
func TestDocReadsAChainModuleWhereverItsBytesAre(t *testing.T) {
	for _, tc := range []struct {
		place string
		where string
	}{
		{"vendor", "vendor/, published on gnoland-1"},
		{"assembly", "the assembly, published on gnoland-1"},
	} {
		t.Run(tc.place, func(t *testing.T) {
			root, module := chainLockedRepo(t, tc.place)
			var out, errw bytes.Buffer
			if err := Run([]string{"doc", "-C", root, module}, &out, &errw); err != nil {
				t.Fatalf("doc on a chain module in %s: %v\n%s", tc.place, err, errw.String())
			}
			if !strings.Contains(out.String(), "func Sprintf(format string, args ...string) string") {
				t.Errorf("the package was not documented:\n%s", out.String())
			}
			// The diagnostic has to name the place, or the next person debugging
			// this reads "no directory" and goes looking in the wrong one.
			if !strings.Contains(errw.String(), tc.where) {
				t.Errorf("the diagnostic does not say where it read from, want %q:\n%s", tc.where, errw.String())
			}
			// "pinned" was the old word and it is wrong here: nothing pinned a
			// module that was published to a chain.
			if strings.Contains(errw.String(), "pinned") {
				t.Errorf("a chain module is described as pinned:\n%s", errw.String())
			}
		})
	}
}

// TestDocOnAChainModuleWithNoBytesNamesBothFixes: the error has to be
// reachable. `gnopm sync` alone was the old advice and is only half of it,
// since a self-contained workspace wants the copy committed.
func TestDocOnAChainModuleWithNoBytesNamesBothFixes(t *testing.T) {
	root, module := chainLockedRepo(t, "")
	var out, errw bytes.Buffer
	err := Run([]string{"doc", "-C", root, module}, &out, &errw)
	if err == nil {
		t.Fatal("doc worked with the bytes nowhere")
	}
	for _, want := range []string{"chain gnoland-1", "vendor/", "gnopm sync", "gnopm vendor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
	// And it must not claim history it never had.
	if strings.Contains(err.Error(), "pinned to history") {
		t.Errorf("a chain module is reported as pinned to history: %v", err)
	}
}
