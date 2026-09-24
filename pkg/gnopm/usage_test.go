package gnopm

import (
	"bytes"
	"strings"
	"testing"
)

// TestEveryCommandIsInAGroup is the guard the grouped usage text needs.
//
// usage() now prints commands under headings, so a command whose group is
// unset, or set to a heading that does not exist, silently does not appear at
// all. That is worse than the flat list it replaced: the command works, it is
// just invisible, and nothing else would notice.
func TestEveryCommandIsInAGroup(t *testing.T) {
	known := map[string]bool{}
	for _, g := range groups {
		known[g.title] = true
	}
	var out bytes.Buffer
	usage(&out)
	text := out.String()

	for _, c := range commands {
		if c.group == "" {
			t.Errorf("command %q has no group, so it is missing from `gnopm help`", c.name)
			continue
		}
		if !known[c.group] {
			t.Errorf("command %q is in group %q, which is not in groups", c.name, c.group)
			continue
		}
		if !strings.Contains(text, "\n  "+c.name+" ") && !strings.Contains(text, "\n  "+c.name+"\n") {
			t.Errorf("command %q is not in the usage text:\n%s", c.name, text)
		}
	}
	for _, g := range groups {
		if !strings.Contains(text, g.title+":") {
			t.Errorf("group %q has no commands, so it should not exist", g.title)
		}
	}
}

// TestGlobalOptionsAreOneList: the usage text, the parser that hoists them past
// the command name, the error naming them, and shell completion all read the
// same slice.
//
// They used to be three hard-coded lists, which is how -v and -no-cache came to
// be missing from the error that tells you which options are global.
func TestGlobalOptionsAreOneList(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	var usageOut bytes.Buffer
	usage(&usageOut)

	completion := map[string]bool{}
	for _, c := range globalFlagCandidates() {
		completion[c.value] = true
	}

	for _, g := range globalOptions {
		t.Run(g.name, func(t *testing.T) {
			if !strings.Contains(usageOut.String(), "-"+g.name) {
				t.Errorf("-%s is not in the usage text", g.name)
			}
			if !completion["-"+g.name] {
				t.Errorf("-%s is not offered by completion", g.name)
			}
			// It has to actually be accepted in front of the command name,
			// which is the whole reason hoistGlobals exists.
			args := []string{"-" + g.name}
			if g.arg != "" {
				args = append(args, placeholderFor(g.name, root))
			}
			args = append(args, "ls", "-C", root)
			var out, errw bytes.Buffer
			if err := Run(args, &out, &errw); err != nil {
				t.Errorf("-%s before the command name: %v\n%s", g.name, err, errw.String())
			}
		})
	}

	// And an option that is not global says so, listing the ones that are.
	var out, errw bytes.Buffer
	err := Run([]string{"-force", "bump", "-C", root, "p/a"}, &out, &errw)
	if err == nil {
		t.Fatal("a command flag was accepted in front of the command name")
	}
	for _, g := range globalOptions {
		if !strings.Contains(err.Error(), "-"+g.name) {
			t.Fatalf("the error does not name -%s: %v", g.name, err)
		}
	}
}

func placeholderFor(name, root string) string {
	if name == "C" {
		return root
	}
	return "{{.Module}}"
}

// TestRecordCommandsIsGenerated: the -f refusal lists the commands -f works on,
// and a hand-written list goes stale. That one already had, naming five while
// six were true.
func TestRecordCommandsIsGenerated(t *testing.T) {
	root := newRepo(t)
	addPkg(t, root, "p/a", "gno.land/p/a/v0", "package a\n")
	commit(t, root, "initial")
	mustRun(t, root, "sync")

	var out, errw bytes.Buffer
	err := Run([]string{"sync", "-C", root, "-f", "{{.Module}}"}, &out, &errw)
	if err == nil {
		t.Fatal("-f on a command with no records was accepted")
	}
	for _, c := range commands {
		named := strings.Contains(err.Error(), " "+c.name) || strings.Contains(err.Error(), ", "+c.name)
		if c.records && !named {
			t.Errorf("%q takes -f but the refusal does not list it: %v", c.name, err)
		}
	}
}

// TestSuggest covers the two ways a command name goes wrong.
func TestSuggest(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		// A transposition, which the previous common-prefix scoring could not
		// see at all: "grpah" shares one letter with "graph".
		{"grpah", []string{"graph"}},
		{"puglish", []string{"publish"}},
		{"statuss", []string{"status"}},
		// An abbreviation is not a typo, and saying both beats picking one.
		{"ver", []string{"verify", "version"}},
		{"stat", []string{"status"}},
		// Through an alias: `deploy` is publish.
		{"depoy", []string{"publish"}},
		// Nonsense gets no guess, because a wrong guess is worse than none.
		{"xyzzy", nil},
		{"", nil},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got := suggest(tc.in)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("suggest(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestEditDistance(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"graph", "graph", 0},
		{"grpah", "graph", 2},
		{"publish", "puglish", 1},
		{"", "ls", 2},
		{"ls", "", 2},
	} {
		if got := editDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestPhrase(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"graph"}, "`gnopm graph`"},
		{[]string{"verify", "version"}, "`gnopm verify` or `gnopm version`"},
		{[]string{"a", "b", "c"}, "`gnopm a`, `gnopm b` or `gnopm c`"},
	} {
		if got := phrase(tc.in); got != tc.want {
			t.Errorf("phrase(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNoUsageLineEndsInWhitespace walks every registered command rather than
// naming the eight that were wrong, so adding a command cannot bring this back.
//
// helpFor interpolated c.args unconditionally, and c.args is empty for every
// command with no positional argument, so `gnopm help sync` printed
// "gnopm sync " with a trailing space. Invisible to read, and enough to stop a
// help text from being compared against a golden file byte for byte.
func TestNoUsageLineEndsInWhitespace(t *testing.T) {
	for _, c := range commands {
		var out bytes.Buffer
		helpFor(&out, c)
		for i, line := range strings.Split(out.String(), "\n") {
			if line != strings.TrimRight(line, " \t") {
				t.Errorf("gnopm help %s, line %d ends in whitespace: %q", c.name, i+1, line)
			}
		}
	}
}
