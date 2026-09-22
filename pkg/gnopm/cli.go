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
	// Verbose says what is being checked, and where each answer came from.
	// Everything it writes goes to Errw, so -v never reaches the pipe.
	Verbose bool
	// CacheDir is where chain answers persist between runs. "" is a cache that
	// remembers nothing: -no-cache, GNOPM_CACHE=off, or a machine with no home
	// directory. Left empty by anything that builds an Env directly, so a
	// caller has to opt in rather than inherit a user's cache by accident.
	CacheDir string
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

// tracef is what -v prints: the running commentary on what is being checked.
//
// Nil-safe on every field, because the things that trace are also the things a
// caller constructs an Env for by hand, and a verbose flag is never worth a
// panic.
func (e *Env) tracef(format string, a ...any) {
	if e == nil || !e.Verbose || e.Errw == nil {
		return
	}
	fmt.Fprintf(e.Errw, format, a...)
}

// cacheDir is where this run may persist chain answers, "" for none.
func (e *Env) cacheDir() string {
	if e == nil {
		return ""
	}
	return e.CacheDir
}

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

Offline and literal: it does what you asked. -if-published is the
opt-in that lets it decide for itself.

  -to <n>         bump to v<n> instead of the next one
  -force          allow uncommitted changes in the package
  -if-published   bump only if the outgoing version is live or parked on
                  the chain its path names. A version nobody could import
                  is not worth a number, so an absent one is left alone
                  for you to edit in place, and the command exits 0
                  having done nothing. Costs one chain read.
  -rpc, -chainid  skip chain discovery, for a local gnodev`,
			flags: func(fs *flag.FlagSet) {
				fs.Int("to", 0, "bump to this major version instead of the next one")
				fs.Bool("force", false, "allow uncommitted changes in the package")
				fs.Bool("if-published", false, "bump only if the outgoing version is on the chain")
				fs.String("rpc", "", "RPC endpoint (default: discovered from the package path)")
				fs.String("chainid", "", "chain id (default: discovered from the package path)")
			},
			run: cmdBump,
		},
		{
			name: "unbump", args: "<package>",
			short: "fold an unpublished version back into the one before it",
			long: `The inverse of bump, for the bump that should not have happened.

Two unpublished versions in a row is a number spent on nothing. A stack
of pull requests produces it by default: the first bumps v0 to v1 and
lands, the second is written against it, sees v1 taken and bumps to v2,
and the package reaches a chain as v2 with v1 existing nowhere.

unbump moves the module line back down and leaves the files alone, so
the work lands inside the version that had not shipped yet.

It reads the chain first and refuses in both directions, because a chain
takes nothing back: a published version cannot be withdrawn, and a
published version cannot be redefined by folding into it either.

  -force          skip the chain check, for an unreachable chain
  -rpc, -chainid  skip chain discovery, for a local gnodev`,
			flags: func(fs *flag.FlagSet) {
				fs.Bool("force", false, "skip the chain check")
				fs.String("rpc", "", "RPC endpoint (default: discovered from the package path)")
				fs.String("chainid", "", "chain id (default: discovered from the package path)")
			},
			run: cmdUnbump,
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
			name: "why", args: "<module>",
			short: "who still imports this version",
			long: `Lists the modules that import <module>, one per line.

The question bump raises and the question tidy answers silently, so it is
worth being able to ask directly: before dropping a pinned version you
want to know what would stop resolving.

<module> is a module path, a directory, or any unambiguous part of one.
With no argument, the package the working directory is in. It is resolved
against the lock rather than against the tree, so a version that no longer
has a directory can still be asked about.

Both halves of the workspace are searched: the working tree and the
materialized assembly. A superseded version importing an older one is how
a chain of versions stays alive, and a graph built from the tree alone
would report that older version as unused.

Silent on stdout when nothing imports it, so an empty answer pipes as
empty.

  -q      importer paths only, one per line
  -json   the module and its importers`,
			run: cmdWhy,
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
directory, and what it names brings its dependencies with it: a package
cannot go up before what it imports, so naming one realm plans whatever it
imports from this workspace too, ahead of it. Only an import that is in
neither this workspace nor the chain stops the plan. Only packages whose
source is in the working tree are considered: a version pinned to history
exists to keep imports resolving.

The chain is read in one concurrent batch behind a progress bar on stderr,
not one blocking query per package.

The chain is discovered from the package path, so gno.land/... resolves to
https://gno.land and the rpc and chain id it advertises. Override with -rpc
and -chainid for a local gnodev.

Asking a chain about two hundred packages is two hundred round trips, so the
one answer that cannot change is kept between runs. A path live on a chain
stays live, so ~/.gnopm/live/<chain-id> remembers it and later runs only ask
about what is missing. Parked and absent are never remembered: a parked
submission can still be enabled or rejected, and absent is the state of the
version you are about to publish.

  -key      gnokey key name for the emitted commands (default: the namespace
            in the package path, since a namespace is its owner)
  -rpc         RPC endpoint, skipping discovery
  -chainid     chain id, skipping discovery
  -gnokey-cmd  the client to emit, if not "gnokey": a wrapper, a path, or
               anything taking the same arguments
  -v           say what is checked and whether the chain or the cache answered
  -no-cache    ask the chain everything, ignoring ~/.gnopm`,
			flags: func(fs *flag.FlagSet) {
				fs.String("key", "", "gnokey key name (default: the namespace in the package path)")
				fs.String("rpc", "", "RPC endpoint (default: discovered from the package path)")
				fs.String("chainid", "", "chain id (default: discovered from the package path)")
				fs.String("gnokey-cmd", "", `the client command to emit (default "gnokey")`)
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
			short: "make the whole workspace right, however long it takes",
			long: `The heavy one. sync is the cheap, silent, idempotent command a
Makefile prerequisite calls; tidy is the one you run when you want it
right, and it is allowed to cost git walks and chain reads.

Four passes:

  1. sync, so the lock describes the working tree and everything it
     pins is materialized.
  2. drop a pinned version when nothing imports it AND its commit never
     reached the default branch, which together mean it existed for
     nobody outside the branch that made it.
  3. verify, the expensive proof: every pinned version is re-read out of
     git history and re-hashed, so a rewritten or garbage-collected
     commit is caught here rather than by whoever's build breaks next.
     It runs after the drop, so it proves the tidied lock.
  4. ask the chain which of these version numbers ever meant anything.
     A version can sit on the default branch for months having been
     published to nobody, and no local check can see that. Where a
     version is absent and so is the one below it, the bump between them
     was spent on nothing, and tidy names the unbump that folds it back.

It writes only what is safe to write unasked. Pass 4 reports: folding a
version away changes a package's identity, so that stays a deliberate
gnopm unbump, one package at a time.

  -n              print what would change and change nothing
  -offline        skip the chain pass
  -rpc, -chainid  skip chain discovery, for a local gnodev`,
			flags: func(fs *flag.FlagSet) {
				fs.Bool("n", false, "print what would change and change nothing")
				fs.Bool("offline", false, "skip the chain pass")
				fs.String("rpc", "", "RPC endpoint (default: discovered from the package path)")
				fs.String("chainid", "", "chain id (default: discovered from the package path)")
			},
			run: func(e *Env, fs *flag.FlagSet, args []string) error {
				return Tidy(e, TidyOptions{
					DryRun:  flagBool(fs, "n"),
					Offline: flagBool(fs, "offline"),
					RPC:     flagString(fs, "rpc"),
					ChainID: flagString(fs, "chainid"),
				})
			},
		},
		{
			name:     "clean",
			anywhere: true,
			short:    "remove what gnopm generated and can rebuild",
			long: `Removes the assembly directory, which holds the versions rebuilt from
git history. Nothing there is source: ` + "`gnopm sync`" + ` rebuilds it byte for
byte from gnomod.lock.

The alternative is a line of rm -rf in every adopting repository's
Makefile, which is the wrong owner: gnopm created the directory and gnopm
knows its name.

  -n       print what would go and remove nothing
  -cache   also remove the shared chain cache (~/.gnopm), the way
           go clean -cache does. Off by default: the assembly belongs to
           this workspace, the cache is shared with every other one on the
           machine. On its own it needs no workspace.`,
			flags: func(fs *flag.FlagSet) {
				fs.Bool("n", false, "print what would go and remove nothing")
				fs.Bool("cache", false, "also remove the shared chain cache")
			},
			run: cmdClean,
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
version reproduces from history, that no pin would be discarded by a
squash merge, and that the branch did not edit a version the chain has
already published. Exits non-zero when one of the first three fails.

The published-edit check WARNS rather than fails, because a repository
can have a fair reason to touch a published version and a red build for
it teaches people to skip the check. It costs one chain read, and skips
itself when there is no chain to reach.

The report goes to stdout and, in GitHub Actions, to the job summary.

  --comment       post it as one sticky pull request comment, updated in
                  place on later runs. Needs GITHUB_TOKEN.
  -rpc, -chainid  skip chain discovery, for a local gnodev.

The provider argument is optional; GitHub is detected from the
environment.`,
			flags: func(fs *flag.FlagSet) {
				fs.Bool("comment", false, "post the report as a sticky pull request comment")
				fs.String("base", "", "ref to compare against (detected by default)")
				fs.String("rpc", "", "chain RPC endpoint, skipping discovery")
				fs.String("chainid", "", "chain id, skipping discovery")
			},
			run: func(e *Env, fs *flag.FlagSet, args []string) error {
				if len(args) > 0 && args[0] != "ci" && args[0] != "github" {
					return fmt.Errorf("unknown tool %q. The only tool today is `ci`", args[0])
				}
				return CI(e, CIOptions{
					Comment: flagBool(fs, "comment"),
					Base:    fs.Lookup("base").Value.String(),
					RPC:     fs.Lookup("rpc").Value.String(),
					ChainID: fs.Lookup("chainid").Value.String(),
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
	fmt.Fprint(w, "  -v         say what is being checked, and where each answer came from\n")
	fmt.Fprint(w, "  -no-cache  ask the chain everything, ignoring ~/.gnopm\n")
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
		if a == "--version" {
			return "version", globals, nil, nil
		}
		// -v is the version when it is the whole command line, and the verbose
		// flag when a command follows it. Nothing is lost: there is nothing to
		// be verbose about when there is no command, and --version is never
		// ambiguous either way.
		if a == "-v" {
			if i+1 >= len(args) {
				return "version", globals, nil, nil
			}
			globals = append(globals, a)
			i++
			continue
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
		case "q", "json", "v", "no-cache":
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
	verbose := fs.Bool("v", false, "say what is being checked, and where each answer came from")
	noCache := fs.Bool("no-cache", false, "ask the chain everything, ignoring the cache")
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
	e := &Env{Root: root, Out: out, Errw: errw, JSON: *jsonOut, Quiet: *quiet, Verbose: *verbose}
	if !*noCache {
		e.CacheDir = cacheDir()
	}
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
