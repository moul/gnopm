package gnopm

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Shell completion.
//
// The scripts are thin on purpose: all three ask the binary what the
// candidates are, through the hidden `gnopm __complete` verb, and do nothing
// but render the answer. Writing the logic three times in three shell dialects
// is how a completion script ends up offering a flag that was removed two
// releases ago, and there is no way to test it.
//
// What earns the feature is not the command names, which are a dozen strings
// anyone can remember. It is the package names: gnopm already resolves them
// fuzzily, so completing `gnopm bump md` out of the workspace is the case worth
// having, and it cannot be done by a static script at all.

// completeVerb is the hidden entry point the scripts call. Deliberately not a
// command: its arguments are the command line being typed, so they must never
// reach gnopm's own flag parser.
const completeVerb = "__complete"

// candidate is one completion, with the description shells that show one want.
type candidate struct{ value, desc string }

// Completion prints the completion script for one shell.
func Completion(e *Env, shell string) error {
	if shell == "" {
		// Detect, do not ask: $SHELL is right almost always, and being wrong
		// costs one retyped word rather than a broken shell.
		shell = detectShell()
		if shell == "" {
			return fmt.Errorf("cannot tell which shell this is from $SHELL: name one of bash, zsh, fish")
		}
		e.logf("detected %s from $SHELL\n", shell)
	}
	script, ok := completionScripts[shell]
	if !ok {
		return fmt.Errorf("no completion for %q. gnopm has bash, zsh and fish", shell)
	}
	e.printf("%s", script)
	return nil
}

func detectShell() string {
	base := filepath.Base(os.Getenv("SHELL"))
	switch base {
	case "bash", "zsh", "fish":
		return base
	}
	return ""
}

// Complete prints the candidates for the words typed so far, one per line as
// "value\tdescription".
//
// The tab-separated shape is fish's native completion format, which zsh's
// _describe also wants and bash simply cuts at the tab. One protocol, three
// renderings.
//
// Every failure here is silent and prints nothing. A completion that writes an
// error message dumps it into the middle of the command line being typed, which
// is worse than offering nothing: outside a workspace, mid-rename, or with an
// unreadable lock, the right answer is no candidates.
func Complete(out io.Writer, words []string) error {
	for _, c := range completionsFor(words) {
		if c.desc == "" {
			fmt.Fprintln(out, c.value)
			continue
		}
		fmt.Fprintf(out, "%s\t%s\n", c.value, c.desc)
	}
	return nil
}

func completionsFor(words []string) []candidate {
	// The last word is the one being typed, and may be empty.
	cur := ""
	var prev []string
	if len(words) > 0 {
		cur, prev = words[len(words)-1], words[:len(words)-1]
	}

	name, _, _, err := hoistGlobals(prev)
	if err != nil {
		return nil
	}
	c := lookup(name)
	if name == "" || c == nil && name != "help" {
		// No command yet. `gnopm <TAB>` is the case that has to work before
		// anything else does.
		if strings.HasPrefix(cur, "-") {
			return filterFlags(globalFlagCandidates(), cur)
		}
		return filter(commandCandidates(), cur)
	}

	// After a flag that takes a value, offer nothing and let the shell fall
	// back to filenames: -C wants a directory and -f wants a template, and
	// guessing at either is worse than the shell's own default.
	if c != nil && len(prev) > 0 && takesValue(c, prev[len(prev)-1]) {
		return nil
	}
	if strings.HasPrefix(cur, "-") {
		return filterFlags(flagCandidates(c), cur)
	}

	switch name {
	case "help":
		return filter(commandCandidates(), cur)
	case "completion":
		return filter([]candidate{
			{"bash", "bash completion script"},
			{"zsh", "zsh completion script"},
			{"fish", "fish completion script"},
		}, cur)
	case "tool":
		return filter([]candidate{{"ci", "check this repository and report"}}, cur)
	}
	if c != nil && c.completesModules {
		return filter(moduleCandidates(prev), cur)
	}
	return nil
}

// moduleCandidates offers every module path and every package directory the
// lock knows.
//
// Both, because every command that takes one accepts either, and because the
// prefix the user has already typed picks between them for free: "gno.land/"
// can only be a module and "p/" can only be a directory.
func moduleCandidates(prev []string) []candidate {
	dir := "."
	for i := 0; i < len(prev)-1; i++ {
		if prev[i] == "-C" || prev[i] == "--C" {
			dir = prev[i+1]
		}
	}
	root, err := FindRoot(dir)
	if err != nil {
		return nil
	}
	lock, err := readLock(root)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []candidate
	for _, en := range lock.Modules {
		if !seen[en.Module] {
			seen[en.Module] = true
			where := en.Source.Dir
			if !en.Source.InTree() {
				where = "pinned to " + short(en.Source.Commit)
			}
			out = append(out, candidate{en.Module, where})
		}
		if d := en.Source.Dir; d != "" && !seen[d] && en.Source.InTree() {
			seen[d] = true
			out = append(out, candidate{d, en.Module})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].value < out[j].value })
	return out
}

func commandCandidates() []candidate {
	out := []candidate{{"help", "help for a command"}}
	for _, c := range commands {
		out = append(out, candidate{c.name, c.short})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].value < out[j].value })
	return out
}

func globalFlagCandidates() []candidate {
	return []candidate{
		{"-C", "run as if started in this directory"},
		{"-f", "go-template applied to each record"},
		{"-json", "machine-readable output"},
		{"-no-cache", "ask the chain everything, ignoring the cache"},
		{"-q", "terse output"},
		{"-v", "say what is being checked, and where each answer came from"},
	}
}

// flagCandidates reads the command's real flag set, so a flag that is added or
// removed shows up in completion the same day rather than whenever somebody
// remembers the script.
func flagCandidates(c *command) []candidate {
	var out []candidate
	newFlagSet(c, io.Discard).VisitAll(func(f *flag.Flag) {
		out = append(out, candidate{"-" + f.Name, f.Usage})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].value < out[j].value })
	return out
}

// takesValue reports whether word is a flag of c that consumes the next word.
func takesValue(c *command, word string) bool {
	if !strings.HasPrefix(word, "-") || strings.Contains(word, "=") {
		return false
	}
	f := newFlagSet(c, io.Discard).Lookup(strings.TrimLeft(word, "-"))
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !b.IsBoolFlag()
}

func filter(all []candidate, prefix string) []candidate {
	var out []candidate
	for _, c := range all {
		if strings.HasPrefix(c.value, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// filterFlags matches a flag with either one dash or two, because both are the
// same flag to Go's flag package and a shell user types whichever they know.
func filterFlags(all []candidate, prefix string) []candidate {
	bare := strings.TrimLeft(prefix, "-")
	var out []candidate
	for _, c := range all {
		if strings.HasPrefix(strings.TrimLeft(c.value, "-"), bare) {
			out = append(out, c)
		}
	}
	return out
}

var completionScripts = map[string]string{
	"bash": `# gnopm bash completion. Install with:
#   gnopm completion bash > /etc/bash_completion.d/gnopm
#   or, per shell:  eval "$(gnopm completion bash)"
_gnopm() {
    local out line
    COMPREPLY=()
    out="$(gnopm __complete "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null)" || return 0
    while IFS=$'\t' read -r line _; do
        [ -n "$line" ] && COMPREPLY+=("$line")
    done <<< "$out"
    return 0
}
# -o default falls back to filenames when gnopm offers nothing, which is what
# -C wants.
complete -o default -F _gnopm gnopm
`,
	"zsh": `#compdef gnopm
# gnopm zsh completion. Install with:
#   gnopm completion zsh > "${fpath[1]}/_gnopm"
#   or, per shell:  eval "$(gnopm completion zsh)"
_gnopm() {
    local -a lines described values
    local line value desc
    # (@) keeps the empty current word, which is what tells gnopm whether you
    # are completing a new word or extending the one under the cursor.
    lines=("${(@f)$(gnopm __complete "${(@)words[2,$CURRENT]}" 2>/dev/null)}")
    for line in $lines; do
        value="${line%%$'\t'*}"
        desc="${line#*$'\t'}"
        [[ -z $value ]] && continue
        values+=("$value")
        if [[ $desc == $line ]]; then
            described+=("$value")
        else
            described+=("${value}:${desc}")
        fi
    done
    (( ${#values} )) || { _files; return }
    _describe -t gnopm 'gnopm' described
}
compdef _gnopm gnopm
`,
	"fish": `# gnopm fish completion. Install with:
#   gnopm completion fish > ~/.config/fish/completions/gnopm.fish
#   or, per shell:  gnopm completion fish | source
function __gnopm_complete
    set -l tokens (commandline -opc)
    set -l cur (commandline -ct)
    # value<TAB>description is fish's own completion format, so this is a
    # straight pass-through.
    gnopm __complete $tokens[2..-1] "$cur" 2>/dev/null
end
complete -c gnopm -f -a '(__gnopm_complete)'
`,
}
