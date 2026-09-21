package gnopm

import (
	"flag"
	"fmt"
	"io"
	"runtime/debug"
	"sort"
	"strings"
)

// buildVersion reports the module version and VCS revision stamped into the
// binary by the Go toolchain.
func buildVersion() (version, revision string, dirty bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)", "", false
	}
	version = info.Main.Version
	if version == "" || version == "(devel)" {
		version = "devel"
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 12 {
				revision = s.Value[:12]
			} else {
				revision = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return version, revision, dirty
}

// env is what every command gets: where the workspace is, where to write data,
// and where to write everything that is not data.
//
// The split is the whole reason `gnopm ls -q | xargs ...` works. Data goes to
// stdout and nothing else ever does, so a pipe receives module paths and not
// progress lines. Diagnostics, progress and warnings go to stderr, where they
// stay visible to a human and invisible to the pipe.
type Env struct {
	// Root is the workspace root: the directory holding gnowork.toml.
	Root string
	// Out receives data and nothing else, so callers can pipe it.
	Out io.Writer
	// Errw receives progress, warnings and diagnostics.
	Errw io.Writer
	// JSON and Quiet select the output shape.
	JSON, Quiet bool
}

// NewEnv locates the workspace containing dir and returns an Env for it.
func NewEnv(dir string, out, errw io.Writer) (*Env, error) {
	root, err := FindRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Env{Root: root, Out: out, Errw: errw}, nil
}

func (e *Env) logf(format string, a ...any)   { fmt.Fprintf(e.Errw, format, a...) }
func (e *Env) printf(format string, a ...any) { fmt.Fprintf(e.Out, format, a...) }

// command is one subcommand.
type command struct {
	name    string
	aliases []string
	args    string
	short   string
	long    string
	flags   func(*flag.FlagSet) // command-specific flags, may be nil
	// anywhere marks a command that does not need a workspace. `gnopm
	// version` failing with "gnowork.toml not found" is absurd, and it is the
	// first thing anyone runs after installing.
	anywhere bool
	run      func(*Env, *flag.FlagSet, []string) error
}

var commands []*command

func init() {
	commands = []*command{
		{
			name: "status", aliases: []string{"st"},
			short: "show what the workspace resolves, and whether anything is out of date",
			long: `Reports how many modules the workspace can resolve, where they come
from, and whether the lock and the assembly are current.

Cheap and read-only. When something is out of date it says so and names
the command that fixes it, rather than fixing it behind your back.`,
			run: func(e *Env, fs *flag.FlagSet, args []string) error { return Status(e) },
		},
		{
			name: "sync", aliases: []string{"up"},
			short: "make the state good: lock what is in the tree, materialize what is not",
			long: `The one maintenance command. After it returns, the workspace is
consistent: gnomod.lock describes the working tree, every version it
pins to history is materialized under the assembly directory, and
anything the lock no longer mentions is gone.

Idempotent and silent when there is nothing to do, so it is safe on a
Makefile prerequisite, a git hook, or every save.

Run it after adding or removing a package. bump runs it for you.`,
			run: func(e *Env, fs *flag.FlagSet, args []string) error { return Sync(e) },
		},
		{
			name: "bump", args: "<package>",
			short: "promote a package to its next version, in place",
			long: `Pins the outgoing version to a commit that still holds it, rewrites
the module line in gnomod.toml, and leaves the directory exactly where
it is. Then you edit the files and git diffs them properly.

<package> is a directory, a module path, or any unambiguous part of one:
"p/alice/md", "gno.land/p/alice/md/v0" and "md" all work.

Syncs before and after, so whatever still imports the outgoing version
resolves again without a second command.

  -to <n>  bump to v<n> instead of the next one
  -force   allow uncommitted changes in the package`,
			flags: func(fs *flag.FlagSet) {
				fs.Int("to", 0, "bump to this major version instead of the next one")
				fs.Bool("force", false, "allow uncommitted changes in the package")
			},
			run: cmdBump,
		},
		{
			name: "ls", aliases: []string{"list"}, args: "[pattern]",
			short: "list resolvable modules and where their source is",
			long: `Lists every module in the lock. An optional pattern filters by
substring against the module path or its directory.

  -q       module paths only, one per line, for piping
  -json    full records as JSON
  -pinned  only versions materialized from history
  -tree    only versions in the working tree`,
			flags: func(fs *flag.FlagSet) {
				fs.Bool("pinned", false, "only versions pinned to history")
				fs.Bool("tree", false, "only versions in the working tree")
			},
			run: cmdLs,
		},
		{
			name: "publish", aliases: []string{"deploy"}, args: "[pattern]",
			short: "what is missing on the chain, in dependency order, as gnokey commands",
			long: `Reads the chain the package paths point at, reports what is live,
parked or absent there, and writes a shell script of gnokey commands for
whatever is missing, ordered so a dependency goes up before its dependents.

gnopm never signs and never broadcasts. The script goes to stdout and the
report to stderr, so it can be reviewed and then piped:

  gnopm publish                    # read the report, read the script
  gnopm publish -key alice | sh    # run it, once you have read it

An optional pattern filters by substring against the module path or its
directory. Only packages whose source is in the working tree are considered:
a version pinned to history exists to keep imports resolving.

The chain is discovered from the package path, so gno.land/... resolves to
https://gno.land and the rpc and chain id it advertises. Override with -rpc
and -chainid for a local gnodev.

  -key      gnokey key name to put in the emitted commands (default "$KEY")
  -rpc      RPC endpoint, skipping discovery
  -chainid  chain id, skipping discovery`,
			flags: func(fs *flag.FlagSet) {
				fs.String("key", "", "gnokey key name for the emitted commands")
				fs.String("rpc", "", "RPC endpoint (default: discovered from the package path)")
				fs.String("chainid", "", "chain id (default: discovered from the package path)")
			},
			run: cmdPublish,
		},
		{
			name: "verify", aliases: []string{"check"},
			short: "prove every pinned version still reproduces (the CI guard)",
			long: `Writes nothing, exits non-zero with what to run.

Where status takes a cheap glance, verify does the expensive proof: it
re-reads every pinned version out of git history and re-hashes it, so a
rewritten or garbage-collected commit is caught rather than discovered
much later by somebody whose build stopped working.

Needs full git history. A shallow clone has none of the pinned commits.

  -upstream <ref>  additionally require every pinned commit to be reachable
                   from <ref>. A pin to a commit that exists only on this
                   branch dies when the branch is squash-merged, so CI runs
                   this with -upstream origin/main.`,
			flags: func(fs *flag.FlagSet) {
				fs.String("upstream", "", "also require every pin to be reachable from this ref (for CI on a pull request)")
			},
			run: func(e *Env, fs *flag.FlagSet, args []string) error {
				return VerifyWith(e, fs.Lookup("upstream").Value.String())
			},
		},
		{
			name:  "tidy",
			short: "drop pinned versions nothing imports that never shipped",
			long: `Removes a lock entry when BOTH are true: nothing in the workspace
imports that version, and the commit it is pinned to never reached the
default branch.

Both, not either. A version nobody here imports may still be deployed
and imported by somebody else, so "unused" alone is not permission to
forget it. A version pinned to a commit that never reached the default
branch, though, never existed for anyone outside the branch that made
it.

That is the normal end state of a branch that adds v0 and then bumps to
v1 before either has landed: the intermediate version is an editing
artefact, not a release.

  -n   print what would go and change nothing`,
			flags: func(fs *flag.FlagSet) {
				fs.Bool("n", false, "print what would go and change nothing")
			},
			run: func(e *Env, fs *flag.FlagSet, args []string) error { return Tidy(e, flagBool(fs, "n")) },
		},
		{
			name:  "env",
			short: "show what gnopm worked out about this workspace",
			long: `Everything gnopm detected rather than was told: the workspace root,
the lock, the assembly, the upstream ref verify checks against, and the
gno home it caches beside.

If a command surprises you, this shows the inputs it surprised you
with.`,
			run: func(e *Env, fs *flag.FlagSet, args []string) error { return cmdEnv(e) },
		},
		{
			name:     "version",
			anywhere: true,
			short:    "print the gnopm version",
			run:      func(e *Env, fs *flag.FlagSet, args []string) error { return cmdVersion(e) },
		},
		{
			name: "tool", args: "ci [github]",
			short: "run a gnopm tool: `ci` checks a repository and reports",
			long: `gnopm tool ci

Runs the checks a repository actually wants in CI and writes a Markdown
report: that the lock describes the working tree, that every pinned
version reproduces from history, and that no pin would be discarded by a
squash merge. Exits non-zero when one fails.

The report goes to stdout and, in GitHub Actions, to the job summary.

  --comment   post it as one sticky pull request comment, updated in
              place on later runs. Needs GITHUB_TOKEN.

The provider argument is optional; GitHub is detected from the
environment.`,
			flags: func(fs *flag.FlagSet) {
				fs.Bool("comment", false, "post the report as a sticky pull request comment")
				fs.String("base", "", "ref to compare against (detected by default)")
			},
			run: func(e *Env, fs *flag.FlagSet, args []string) error {
				if len(args) > 0 && args[0] != "ci" && args[0] != "github" {
					return fmt.Errorf("unknown tool %q. The only tool today is `ci`", args[0])
				}
				return CI(e, CIOptions{
					Comment: flagBool(fs, "comment"),
					Base:    fs.Lookup("base").Value.String(),
				})
			},
		},
		{
			name:  "badges",
			short: "shields.io badges describing this workspace",
			long: `Markdown by default, the shields endpoint shape with -json.

Generated rather than hand-written, because a hand-written badge is a
claim nobody re-checks.`,
			run: func(e *Env, fs *flag.FlagSet, args []string) error { return Badges(e, e.JSON) },
		},
		{
			name:  "deversion",
			short: "one-time migration: lift every pkg/vN directory up to pkg",
			long: `For a repository that still keeps each version in its own directory.

Pins every version to a commit first, so nothing can be lost, then lifts
the highest version of each package up a level with git mv so the change
reviews as renames, then re-locks and materializes.

Module lines are never touched, so no package path and no realm address
changes. Idempotent, and only touches directories that still carry a
version, so a branch opened before the migration can rebase and re-run
it to fix up just its own packages.

  -n   print the plan and change nothing`,
			flags: func(fs *flag.FlagSet) {
				fs.Bool("n", false, "print the plan and change nothing")
			},
			run: cmdDeversion,
		},
	}
}

func lookup(name string) *command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
		for _, a := range c.aliases {
			if a == name {
				return c
			}
		}
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprint(w, "gnopm keeps a package's version in gnomod.toml instead of in its directory name.\n\n")
	fmt.Fprint(w, "usage: gnopm <command> [options]\n\n")
	width := 0
	for _, c := range commands {
		if len(c.name) > width {
			width = len(c.name)
		}
	}
	for _, c := range commands {
		fmt.Fprintf(w, "  %-*s  %s\n", width, c.name, c.short)
	}
	fmt.Fprint(w, "\nglobal options:\n")
	fmt.Fprint(w, "  -C <dir>   run as if started in <dir>\n")
	fmt.Fprint(w, "  -q         terse output, module paths only where that makes sense\n")
	fmt.Fprint(w, "  -json      machine-readable output\n")
	fmt.Fprint(w, "\n`gnopm help <command>` for detail. Start with `gnopm status`.\n")
}

func helpFor(w io.Writer, c *command) {
	fmt.Fprintf(w, "gnopm %s %s\n\n%s\n", c.name, c.args, c.short)
	if c.long != "" {
		fmt.Fprintf(w, "\n%s\n", c.long)
	}
	if len(c.aliases) > 0 {
		fmt.Fprintf(w, "\naliases: %s\n", strings.Join(c.aliases, ", "))
	}
}

// suggest finds the closest command name, so a typo is one line of help and
// not a wall of usage.
func suggest(name string) string {
	best, bestScore := "", 0
	for _, c := range commands {
		all := append([]string{c.name}, c.aliases...)
		for _, cand := range all {
			s := commonPrefix(name, cand)
			if s > bestScore {
				best, bestScore = c.name, s
			}
		}
	}
	if bestScore >= 2 {
		return best
	}
	return ""
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// hoistGlobals pulls the global options out from in front of the subcommand,
// so `gnopm -C somewhere status` works the way `git -C somewhere status` does.
//
// Go's flag package only parses flags it meets before the first positional,
// which would make the documented global options usable only AFTER the
// command name. Having `-C` documented as global and then rejected in the one
// position people actually type it is worse than not having it.
//
// globals and rest come back apart rather than already spliced together,
// because help reads its topic straight off the front of rest. Handing it a
// slice that still carried `-C somewhere` made `gnopm -C somewhere help`
// answer `unknown command "-C"`.
func hoistGlobals(args []string) (name string, globals, rest []string, err error) {
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "-" || !strings.HasPrefix(a, "-") {
			break
		}
		if a == "-h" || a == "--help" {
			return "help", globals, args[i+1:], nil
		}
		if a == "-v" || a == "--version" {
			return "version", globals, nil, nil
		}
		opt := strings.TrimLeft(a, "-")
		if strings.Contains(opt, "=") {
			globals = append(globals, a)
			i++
			continue
		}
		switch opt {
		case "C":
			if i+1 >= len(args) {
				return "", nil, nil, fmt.Errorf("-C needs a directory")
			}
			globals = append(globals, a, args[i+1])
			i += 2
		case "q", "json":
			globals = append(globals, a)
			i++
		default:
			return "", nil, nil, fmt.Errorf("%q is not a global option. -C, -q and -json go anywhere; every other option goes after the command name", a)
		}
	}
	if i >= len(args) {
		return "", globals, nil, nil
	}
	return args[i], globals, args[i+1:], nil
}

// hasHelpFlag reports whether the arguments after a command name ask for that
// command's help.
func hasHelpFlag(args []string) bool {
	for _, a := range args {
		switch a {
		case "-h", "--help", "-help":
			return true
		}
	}
	return false
}

func Run(args []string, out, errw io.Writer) error {
	if len(args) == 0 {
		usage(errw)
		return nil
	}
	name, globals, rest, err := hoistGlobals(args)
	if err != nil {
		return err
	}
	if name == "" {
		usage(errw)
		return nil
	}

	if name == "help" || name == "-h" || name == "--help" {
		if len(rest) > 0 {
			if c := lookup(rest[0]); c != nil {
				helpFor(out, c)
				return nil
			}
			return fmt.Errorf("unknown command %q", rest[0])
		}
		usage(out)
		return nil
	}

	c := lookup(name)
	if c == nil {
		if s := suggest(name); s != "" {
			return fmt.Errorf("unknown command %q. Did you mean `gnopm %s`?", name, s)
		}
		return fmt.Errorf("unknown command %q. Run `gnopm help`.", name)
	}

	// -h after the command name is a request for help, not a mistake. Left to
	// the flag package it prints to stderr and exits 1, so `gnopm status -h`
	// disagreed with `gnopm help status` about both stream and exit code.
	if hasHelpFlag(rest) {
		helpFor(out, c)
		return nil
	}

	fs := flag.NewFlagSet("gnopm "+c.name, flag.ContinueOnError)
	fs.SetOutput(errw)
	fs.Usage = func() { helpFor(errw, c) }
	chdir := fs.String("C", ".", "run as if started in this directory")
	jsonOut := fs.Bool("json", false, "machine-readable output")
	quiet := fs.Bool("q", false, "terse output")
	if c.flags != nil {
		c.flags(fs)
	}
	// Flags before or after the positional arguments, because insisting on one
	// order is exactly the kind of thing that makes a CLI annoying.
	positional, err := parseInterspersed(fs, append(globals, rest...))
	if err != nil {
		return err
	}

	root := ""
	if !c.anywhere {
		var err error
		if root, err = FindRoot(*chdir); err != nil {
			return err
		}
	}
	e := &Env{Root: root, Out: out, Errw: errw, JSON: *jsonOut, Quiet: *quiet}
	return c.run(e, fs, positional)
}

// parseInterspersed parses flags that appear anywhere in args and returns the
// positional arguments in order. Go's flag package stops at the first
// non-flag; `gnopm bump md -force` is a perfectly reasonable thing to type.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	pending := args
	for len(pending) > 0 {
		if err := fs.Parse(pending); err != nil {
			return nil, err
		}
		remaining := fs.Args()
		if len(remaining) == 0 {
			break
		}
		positional = append(positional, remaining[0])
		pending = remaining[1:]
	}
	return positional, nil
}

func flagBool(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	return f.Value.String() == "true"
}

func flagString(fs *flag.FlagSet, name string) string {
	f := fs.Lookup(name)
	if f == nil {
		return ""
	}
	return f.Value.String()
}

func flagInt(fs *flag.FlagSet, name string) int {
	f := fs.Lookup(name)
	if f == nil {
		return 0
	}
	var n int
	fmt.Sscanf(f.Value.String(), "%d", &n)
	return n
}

func sortedModules(l *Lock) []LockEntry {
	out := append([]LockEntry(nil), l.Modules...)
	sort.Slice(out, func(i, j int) bool { return out[i].Module < out[j].Module })
	return out
}
