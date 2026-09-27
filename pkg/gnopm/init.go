package gnopm

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// The front door.
//
// gnopm had no init, and neither has anything else: `gno mod init` writes a
// package's gnomod.toml, and nothing at all writes a workspace's gnowork.toml.
// So the first thing a newcomer had to do was know that an empty file with a
// particular name is what makes a directory a workspace, and the error they got
// instead was `gnowork.toml not found in "." or any parent`, which names the
// file without saying it is theirs to create.
//
// The ownership line is clean and worth stating: gno owns the package manifest,
// gnopm owns the workspace around it.

// Init makes the current directory a gno workspace.
//
// Deliberately tiny. It writes the marker, the lock and the ignore rule, and
// then tells you the next thing to type. Scaffolding a first package is `gno
// mod init`'s job and this does not duplicate it: a tool that writes files
// another tool owns is a tool that goes stale the first time that format moves.
func Init(e *Env, dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	marker := filepath.Join(abs, workspaceMarker)
	if _, err := os.Stat(marker); err == nil {
		// Not an error. Running init twice is what somebody does when they are
		// not sure it worked, and the honest answer is that it did.
		e.logf("%s already exists: this is already a workspace.\n", workspaceMarker)
		return nil
	}
	// Refusing inside an existing workspace is the one guard that matters:
	// nested workspaces resolve ambiguously, and the nesting is invisible from
	// the directory you are standing in.
	if root, err := FindRoot(abs); err == nil && root != abs {
		return fmt.Errorf("%s is already inside the workspace at %s.\n"+
			"  Nested workspaces resolve ambiguously. Add a package here instead:\n"+
			"  `gno mod init gno.land/p/<namespace>/<name>/v0`, then `gnopm sync`", abs, root)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	// Empty on purpose, and worth a comment in the file itself: the emptiness
	// IS the design, and a reader who does not know that assumes it is broken.
	// gno resolves every package under this directory by its module line, not
	// by where it sits, which is the whole premise gnopm is built on.
	body := "# This file marks a gno workspace. Empty is correct: gno resolves\n" +
		"# every package underneath it by the module line in its gnomod.toml,\n" +
		"# not by its directory name. Created by `gnopm init`.\n"
	if err := os.WriteFile(marker, []byte(body), 0o644); err != nil {
		return err
	}
	e.logf("created %s\n", workspaceMarker)

	// The lock and the ignore rule come from the machinery that owns them
	// rather than being written here, so init cannot drift from sync.
	inner := *e
	inner.Root = abs
	if err := Sync(&inner); err != nil {
		return err
	}
	// Not `git init`: which VCS a project uses is not gnopm's call, and a tool
	// that creates repositories you did not ask for is a tool people run in
	// the wrong directory once and never again. But gnomod.lock pins
	// superseded versions to commits, so without one `gnopm bump` fails later
	// with an error about git rather than about this moment. Say it now.
	if _, err := os.Stat(filepath.Join(abs, ".git")); err != nil {
		e.logf("\nnote: no git repository here. gnomod.lock pins superseded versions to the\n" +
			"  commits that still hold them, so `gnopm bump` needs one. `git init` when ready.\n")
	}
	e.logf("\nthis is a gno workspace now. next:\n" +
		"  gno mod init gno.land/p/<namespace>/<name>/v0   add your first package\n" +
		"  gnopm get <package-path>                        add one that lives on a chain\n" +
		"  gnopm status                                    ask what resolves\n" +
		"  gnopm help                                      everything else\n",
	)
	return nil
}

func cmdInit(e *Env, fs *flag.FlagSet, args []string) error {
	// -C is "run as if started in <dir>", and init is an `anywhere` command so
	// nothing else has applied it: Env.Root is empty by construction here.
	// Ignoring it made `gnopm init -C /tmp/x` initialise the process's own
	// working directory instead, which is the one place a typo really costs.
	dir := flagString(fs, "C")
	if dir == "" {
		dir = "."
	}
	if len(args) == 1 {
		dir = args[0]
	}
	if len(args) > 1 {
		return fmt.Errorf("init takes one directory at most, got %d", len(args))
	}
	return Init(e, dir)
}

// foreign maps what people type when they arrive from another ecosystem.
//
// Not politeness. Every one of these is a real command somewhere, so a bare
// `unknown command "install". Run gnopm help.` sends somebody to read eighteen
// command names looking for a word that is not there, and the conclusion they
// reach is that the feature is missing rather than unnecessary. Answering with
// the reason is the only chance to explain why an import path carrying its own
// version means there is nothing to resolve.
//
// Keyed by the word, because the same word means different things in different
// ecosystems and the advice is the same either way.
var foreign = map[string]string{
	"install": "nothing to install: an import path IS the address, so a dependency in this\n" +
		"  workspace already resolves. For one that lives only on a chain:\n" +
		"    gnopm get <package-path>   fetch it\n" +
		"    gnopm sync                 make the workspace resolve",
	"add": "gnopm has no `add`. To depend on something, import it: a path in this\n" +
		"  workspace resolves already, and one that lives only on a chain is\n" +
		"    gnopm get <package-path>",
	"i": "gnopm has no `i`. You probably want `gnopm get <package-path>` to fetch a\n" +
		"  dependency from a chain, or `gnopm sync` to make the workspace resolve",
	"remove": "gnopm has no `remove`. Delete the import, then `gnopm tidy`, which drops a\n" +
		"  pinned version nothing imports any more",
	"uninstall": "gnopm has no `uninstall`. Delete the import, then `gnopm tidy`, which drops\n" +
		"  a pinned version nothing imports any more",
	"update": "gnopm has no `update`, and it is not an omission: a gno import path carries\n" +
		"  its version, so nothing updates under you and there is no range to re-solve.\n" +
		"  Moving to a newer version is changing the import",
	"upgrade": "gnopm has no `upgrade`: an import path carries its version, so nothing moves\n" +
		"  under you. Moving to a newer version is changing the import",
	"test":   "gnopm does not build or test; the toolchain does. `gno test ./...`",
	"build":  "gnopm does not build; the toolchain does. `gno build ./...`",
	"run":    "gnopm does not run code; the toolchain does. `gno run`",
	"lint":   "gnopm does not lint; the toolchain does. `gno lint ./...`",
	"fmt":    "gnopm does not format; the toolchain does. `gno fmt`",
	"audit":  "gnopm has no `audit`. There is no registry to have been compromised: the\n  chain is the registry, and a published path can never be redefined",
	"login":  "gnopm has no accounts and holds no keys. Publishing signs with gnokey,\n  which prompts you: `gnopm publish`",
	"whoami": "gnopm has no accounts. The namespace in a package path is its owner",
	"new":    "gnopm has no `new`. `gnopm init` makes a workspace; `gno mod init <path>`\n  makes a package inside it",
}

// translate returns the advice for a word borrowed from another ecosystem.
//
// Only consulted when the word is not a command here, so a real command can
// never be shadowed by this table: the entries naming commands that DO exist
// are there for the day one is renamed, and are unreachable until then.
func translate(name string) (string, bool) {
	s, ok := foreign[name]
	return s, ok
}
