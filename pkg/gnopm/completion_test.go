package gnopm

import (
	"bytes"
	"strings"
	"testing"
)

// TestCompletionsFor is the whole completion protocol in one table.
//
// The three shell scripts render whatever this returns and decide nothing, so
// this table is the only place the behaviour exists and the only place it can
// be tested. A completion bug that lives in a .bash file is a bug nobody finds.
func TestCompletionsFor(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	addPkg(t, root, "r/b", "gno.land/r/b/v0", "package b\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")
	mustRun(t, root, "bump", "p/a")

	// -C is how the test reaches the workspace, exactly as a shell would when
	// completing a command line that already carries one.
	at := func(words ...string) []string {
		full := append([]string{"-C", root}, words...)
		var got []string
		for _, c := range completionsFor(full) {
			got = append(got, c.value)
		}
		return got
	}

	for _, tc := range []struct {
		name  string
		words []string
		want  []string
		gone  []string
	}{
		{"no command yet", []string{""}, []string{"bump", "status", "why", "help"}, nil},
		{"command prefix", []string{"cl"}, []string{"clean"}, []string{"bump", "status"}},
		{"global flags", []string{"-"}, []string{"-C", "-json", "-q", "-f"}, []string{"bump"}},
		{"command flags", []string{"ls", "-"}, []string{"-pinned", "-tree", "-json"}, []string{"-force"}},
		{"flag prefix", []string{"bump", "-if"}, []string{"-if-published"}, []string{"-force"}},
		// A flag written with two dashes is the same flag to Go's flag
		// package, so completion has to agree.
		{"double dash", []string{"bump", "--if"}, []string{"-if-published"}, nil},
		{"help topics", []string{"help", "ve"}, []string{"verify", "version"}, []string{"bump"}},
		{"shells", []string{"completion", ""}, []string{"bash", "zsh", "fish"}, nil},
		{"tools", []string{"tool", ""}, []string{"ci"}, nil},
		// The case that earns the feature: package names out of the lock,
		// including a version that has no directory left.
		{"modules", []string{"bump", ""}, []string{"gno.land/p/a/v0", "gno.land/p/a/v1", "gno.land/r/b/v0", "p/a", "r/b"}, nil},
		{"module prefix", []string{"why", "gno.land/p/a/"}, []string{"gno.land/p/a/v0", "gno.land/p/a/v1"}, []string{"gno.land/r/b/v0"}},
		{"directory prefix", []string{"bump", "r/"}, []string{"r/b"}, []string{"p/a"}},
		// A command with no positional argument offers nothing rather than
		// every module in the workspace.
		{"no positional", []string{"sync", ""}, nil, []string{"gno.land/p/a/v0", "p/a"}},
		// After a flag that takes a value, the shell's own filename fallback
		// is a better answer than any guess gnopm could make.
		{"value flag", []string{"ls", "-f", ""}, nil, []string{"gno.land/p/a/v0", "-json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := at(tc.words...)
			joined := " " + strings.Join(got, " ") + " "
			for _, w := range tc.want {
				if !strings.Contains(joined, " "+w+" ") {
					t.Fatalf("missing %q in %v", w, got)
				}
			}
			for _, w := range tc.gone {
				if strings.Contains(joined, " "+w+" ") {
					t.Fatalf("unwanted %q in %v", w, got)
				}
			}
			if len(tc.want) == 0 && len(got) != 0 {
				t.Fatalf("expected no candidates, got %v", got)
			}
		})
	}
}

// TestCompleteOutsideWorkspace: a completion that errors dumps text into the
// middle of the command line being typed, so every failure has to be silent.
func TestCompleteOutsideWorkspace(t *testing.T) {
	dir := t.TempDir() // no gnowork.toml anywhere above a temp dir
	var out, errw bytes.Buffer
	if err := Run([]string{"__complete", "-C", dir, "bump", ""}, &out, &errw); err != nil {
		t.Fatalf("completion outside a workspace returned an error: %v", err)
	}
	if out.String() != "" || errw.String() != "" {
		t.Fatalf("completion outside a workspace was not silent: %q / %q", out.String(), errw.String())
	}
	// The command names still work there, because they do not need a lock.
	out.Reset()
	if err := Run([]string{"__complete", "-C", dir, "st"}, &out, &errw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "status") {
		t.Fatalf("command names need no workspace, got %q", out.String())
	}
}

// TestCompletionScripts checks the scripts are emitted and that the protocol
// they implement is the one Complete writes.
func TestCompletionScripts(t *testing.T) {
	root := newRepo(t)
	write(t, root+"/gnowork.toml", "")

	for shell, needle := range map[string]string{
		"bash": "complete -o default -F _gnopm gnopm",
		"zsh":  "compdef _gnopm gnopm",
		"fish": "complete -c gnopm",
	} {
		t.Run(shell, func(t *testing.T) {
			var out, errw bytes.Buffer
			if err := Run([]string{"completion", "-C", root, shell}, &out, &errw); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), needle) {
				t.Fatalf("the %s script does not contain %q:\n%s", shell, needle, out.String())
			}
			// Every script calls the same hidden verb, so a rename here
			// cannot silently leave three scripts calling nothing.
			if !strings.Contains(out.String(), "gnopm "+completeVerb) {
				t.Fatalf("the %s script does not call %s", shell, completeVerb)
			}
		})
	}

	// A named shell gnopm does not have is an error, not a silent empty file
	// somebody sources for a year.
	var out, errw bytes.Buffer
	if err := Run([]string{"completion", "-C", root, "tcsh"}, &out, &errw); err == nil {
		t.Fatal("an unknown shell was accepted")
	} else if !strings.Contains(err.Error(), "bash, zsh and fish") {
		t.Fatalf("the refusal does not name the shells: %v", err)
	}

	// Detected from $SHELL when not named, because asking for something the
	// environment already says is friction paid on every invocation.
	t.Setenv("SHELL", "/usr/bin/fish")
	out.Reset()
	errw.Reset()
	if err := Run([]string{"completion", "-C", root}, &out, &errw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "complete -c gnopm") {
		t.Fatalf("$SHELL was not used:\n%s", out.String())
	}
	if !strings.Contains(errw.String(), "detected fish") {
		t.Fatalf("a detected shell has to be said out loud, on stderr: %q", errw.String())
	}
}
