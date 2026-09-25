package gnopm

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestFormat covers -f across every view that has records, because the whole
// value of the flag is that one spelling works everywhere rather than on the
// one command somebody needed it for.
func TestFormat(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	addPkg(t, root, "r/b", "gno.land/r/b/v0", "package b\n\nimport \"gno.land/p/a/v0\"\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	mustRun(t, root, "bump", "p/a")

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"ls", []string{"ls", "-f", "{{.Module}} {{.Source}}"},
			"gno.land/p/a/v0 commit\ngno.land/p/a/v1 dir\ngno.land/r/b/v0 dir\n"},
		{"ls filtered", []string{"ls", "p/a", "-f", "{{.Module}}"},
			"gno.land/p/a/v0\ngno.land/p/a/v1\n"},
		{"why", []string{"why", "gno.land/p/a/v0", "-f", "{{.Importer}} -> {{.Module}}"},
			"gno.land/r/b/v0 -> gno.land/p/a/v0\n"},
		{"status", []string{"status", "-f", "{{.Modules}} {{.Tree}} {{.Pinned}} {{.OK}}"},
			"3 2 1 true\n"},
		{"env", []string{"env", "-f", "{{.Root}}"}, root + "\n"},
		// -format is the spelling people reach for, and it has to mean the
		// same thing rather than be silently ignored.
		{"format alias", []string{"ls", "-format", "{{.Module}}", "-tree"},
			"gno.land/p/a/v1\ngno.land/r/b/v0\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			args := append([]string{tc.args[0], "-C", root}, tc.args[1:]...)
			if err := Run(args, &out, &errw); err != nil {
				t.Fatalf("%v: %v\n%s", tc.args, err, errw.String())
			}
			if out.String() != tc.want {
				t.Fatalf("got %q, want %q", out.String(), tc.want)
			}
		})
	}

	// Before the command name too, the way -C already works.
	var out, errw bytes.Buffer
	if err := Run([]string{"-f", "{{.Module}}", "-C", root, "ls", "-tree"}, &out, &errw); err != nil {
		t.Fatalf("-f before the command: %v\n%s", err, errw.String())
	}
	if out.String() != "gno.land/p/a/v1\ngno.land/r/b/v0\n" {
		t.Fatalf("-f before the command printed %q", out.String())
	}
}

// TestFormatRefusals: the two mistakes worth an error rather than a guess.
func TestFormatRefusals(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	for _, tc := range []struct {
		name, want string
		args       []string
	}{
		// Same refusal `gno list` makes: two output shapes asked for at once
		// is a mistake, and picking one silently means a script gets the other.
		{"with -json", "cannot be used with -json", []string{"ls", "-f", "{{.Module}}", "-json"}},
		// A command with nothing to template accepting the flag and printing
		// its ordinary output would be the worst outcome: silently wrong.
		{"no records", "has no records to template", []string{"sync", "-f", "{{.Module}}"}},
		{"bad template", "parsing -f", []string{"ls", "-f", "{{.Module"}},
		{"unknown field", "applying -f", []string{"ls", "-f", "{{.Nope}}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			args := append([]string{tc.args[0], "-C", root}, tc.args[1:]...)
			err := Run(args, &out, &errw)
			if err == nil {
				t.Fatalf("expected an error mentioning %q, got none", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestJSONShapeUnchanged pins the keys of every -json view.
//
// -f and -json now share one struct per view, so the field a template names is
// the field the JSON carries. That refactor could have renamed a key without
// anything noticing until somebody's jq stopped matching, which is exactly the
// kind of break a lock-adjacent tool cannot afford.
func TestJSONShapeUnchanged(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	mustRun(t, root, "bump", "p/a")

	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"status", []string{"status"}, []string{"modules", "tree", "pinned", "ok", "lock", "assembly"}},
		{"env", []string{"env"}, []string{"GNOPM_ROOT", "GNOPM_LOCK", "GNOPM_ASSEMBLY", "GNOPM_UPSTREAM", "GNOPM_CACHE", "GNOPM_DOWNLOAD", "GNOHOME"}},
		{"version", []string{"version"}, []string{"version", "revision", "dirty"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			args := append([]string{tc.args[0], "-C", root, "-json"}, tc.args[1:]...)
			if err := Run(args, &out, &errw); err != nil {
				t.Fatalf("%v: %v", tc.args, err)
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("not an object: %v\n%s", err, out.String())
			}
			for _, k := range tc.want {
				if _, ok := got[k]; !ok {
					t.Fatalf("key %q went missing:\n%s", k, out.String())
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d keys, want %d:\n%s", len(got), len(tc.want), out.String())
			}
		})
	}

	// ls is a list, and a { dir } entry must still carry no commit and no
	// hash: omitempty there is load-bearing, because a dir entry having a
	// hash is a lock that rewrites itself on every source edit.
	var out, errw bytes.Buffer
	if err := Run([]string{"ls", "-C", root, "-json"}, &out, &errw); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("not a list: %v\n%s", err, out.String())
	}
	for _, r := range rows {
		switch r["source"] {
		case "dir":
			if _, ok := r["commit"]; ok {
				t.Fatalf("a dir entry carries a commit: %v", r)
			}
			if _, ok := r["hash"]; ok {
				t.Fatalf("a dir entry carries a hash: %v", r)
			}
		case "commit":
			for _, k := range []string{"module", "source", "dir", "commit", "hash"} {
				if _, ok := r[k]; !ok {
					t.Fatalf("a commit entry is missing %q: %v", k, r)
				}
			}
		}
	}
}
