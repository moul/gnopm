package gnopm

import (
	"fmt"
	"text/template"
)

// Output records. One type per view, and the same value backs both -json and
// -f, so the field a template names is the field the JSON carries. Two shapes
// for one view is how `-f '{{.Module}}'` ends up printing nothing while
// `-json | jq .module` works.

// LsRecord is one row of `gnopm ls`.
type LsRecord struct {
	Module string `json:"module"`
	// Source names the union arm in gnomod.lock: "dir" for the working tree,
	// "commit" for a version materialized out of history.
	Source string `json:"source"`
	Dir    string `json:"dir"`
	Commit string `json:"commit,omitempty"`
	Hash   string `json:"hash,omitempty"`
}

// StatusRecord is `gnopm status`.
type StatusRecord struct {
	Modules  int    `json:"modules"`
	Tree     int    `json:"tree"`
	Pinned   int    `json:"pinned"`
	OK       bool   `json:"ok"`
	Lock     string `json:"lock"`
	Assembly string `json:"assembly"`
}

// WhyRecord is one importer of the module `gnopm why` was asked about.
type WhyRecord struct {
	Importer string `json:"importer"`
	Module   string `json:"module"`
}

// EnvRecord is `gnopm env`. The JSON keys are the environment-variable names
// the plain output prints, so the two views stay comparable; the field names
// are what a template says.
type EnvRecord struct {
	Root     string `json:"GNOPM_ROOT"`
	Lock     string `json:"GNOPM_LOCK"`
	Assembly string `json:"GNOPM_ASSEMBLY"`
	Upstream string `json:"GNOPM_UPSTREAM"`
	Cache    string `json:"GNOPM_CACHE"`
	// Download is where fetched dependencies land under the cache. Printed
	// separately because it is the one part of the cache that holds source
	// rather than answers, so it is what somebody looks for when they want to
	// read what a dependency actually contains.
	Download string `json:"GNOPM_DOWNLOAD"`
	GnoHome  string `json:"GNOHOME"`
}

// VersionRecord is `gnopm version`.
type VersionRecord struct {
	Version  string `json:"version"`
	Revision string `json:"revision"`
	Dirty    bool   `json:"dirty"`
}

// emit writes one line per record through the -f template.
//
// Same semantics as `gno list -f`, deliberately: text/template, executed once
// per record, one newline appended after each. gnopm having its own spelling of
// an idea the toolchain already has is worse than not having it, so the flag is
// -f and the behaviour is copied rather than improved on.
func (e *Env) emit(records ...any) error {
	tmpl, err := template.New("gnopm-format").Parse(e.Format)
	if err != nil {
		return fmt.Errorf("parsing -f %q: %w", e.Format, err)
	}
	for _, r := range records {
		if err := tmpl.Execute(e.Out, r); err != nil {
			return fmt.Errorf("applying -f to %T: %w", r, err)
		}
		if _, err := fmt.Fprintln(e.Out); err != nil {
			return err
		}
	}
	return nil
}
