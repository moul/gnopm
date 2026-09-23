package gnopm

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Running the plan, rather than printing it for someone to paste.
//
// gnopm still holds no key and signs nothing. What it does now is start the
// client you named, once per command, with your terminal attached, so gnokey
// asks you for the passphrase exactly as it would if you had typed the command
// yourself. That is the whole difference: the signing authority never moves,
// the copy-paste step goes away.
//
// `-print` is the escape hatch and the old default: it emits the same commands
// as a shell script and starts nothing.
//
// A pipe cannot replace this. `gnopm publish | sh` takes stdin away from
// gnokey, so the passphrase prompt fails with "inappropriate ioctl for
// device", which is why that shape was always a footgun and why the script
// form tells you to save it to a file first.

// publishCmd is one client invocation: the binary, its arguments grouped the
// way they are displayed, and the comment that says what it is for.
//
// The grouping exists only so the printed script keeps a flag and its value on
// one line. Execution flattens it, so the two forms can never diverge: there is
// one list of arguments and two renderings of it.
type publishCmd struct {
	note   string     // the "# ..." line above it, already worded, no marker
	name   string     // the client binary, "gnokey" unless -gnokey-cmd said otherwise
	groups [][]string // one display line each
}

func (c publishCmd) flat() []string {
	var out []string
	for _, g := range c.groups {
		out = append(out, g...)
	}
	return out
}

// script renders the commands the way they were always rendered: a shell
// script on stdout, with the report already on stderr, so it can be read,
// saved and run.
func (e *Env) printPublish(cmds []publishCmd, banner string) {
	e.printf("#!/bin/sh\n# %s; review, then save it and run it.\n", banner)
	// Not `| sh`: gnokey reads the passphrase from stdin and a pipe takes
	// stdin away. Saving to a file is the shape that works.
	e.printf("# gnopm signs nothing. Save to a file and run it, do not pipe it\n")
	e.printf("# into sh: that takes stdin away and gnokey cannot prompt.\nset -e\n")
	for _, c := range cmds {
		if c.note != "" {
			e.printf("\n# %s\n", c.note)
		} else {
			e.printf("\n")
		}
		// The first group rides on the command's own line, so the script opens
		// with `gnokey maketx addpkg \` and not a bare `gnokey \`.
		e.printf("%s", c.name)
		for i, g := range c.groups {
			sep := " \\\n  "
			if i == 0 {
				sep = " "
			}
			e.printf("%s%s", sep, strings.Join(quoteAll(g), " "))
		}
		e.printf("\n")
	}
}

func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuoteIfNeeded(a)
	}
	return out
}

// shellQuoteIfNeeded quotes only what a shell would otherwise reinterpret, so
// the printed script stays readable: `-func Set` rather than `'-func' 'Set'`.
func shellQuoteIfNeeded(s string) string {
	if s == "" {
		return "''"
	}
	if strings.ContainsAny(s, " \t\n'\"\\$`&|;<>()*?[]{}#~!") {
		return shellQuote(s)
	}
	return s
}

// startClient is how a publish command is run. A package-level seam rather
// than a parameter threaded through five call sites: tests replace it, and
// nothing else ever should.
var startClient = func(name string, args []string, out, errw *os.File) error {
	cmd := exec.Command(name, args...)
	// The terminal, not a pipe: gnokey prompts for the passphrase on stdin and
	// a pipe is exactly what breaks it.
	cmd.Stdin = os.Stdin
	cmd.Stdout = out
	cmd.Stderr = errw
	return cmd.Run()
}

// runPublish starts each command in order and stops at the first failure.
//
// Stopping is not a policy choice, it is the only correct one: the commands
// are in dependency order, so a package whose dependency did not land cannot
// land either, and continuing would send a transaction that is certain to be
// rejected and certain to be charged for.
func (e *Env) runPublish(cmds []publishCmd) error {
	out, errw := stdFile(e.Out, os.Stdout), stdFile(e.Errw, os.Stderr)
	for i, c := range cmds {
		if c.note != "" {
			e.logf("\nrun      [%d/%d] %s\n", i+1, len(cmds), c.note)
		} else {
			e.logf("\nrun      [%d/%d] %s\n", i+1, len(cmds), c.name)
		}
		e.logf("         %s %s\n", c.name, strings.Join(quoteAll(c.flat()), " "))
		if err := startClient(c.name, c.flat(), out, errw); err != nil {
			// Say how far it got. After a partial run the chain and the plan
			// disagree, and the next thing to do is re-read the chain rather
			// than re-run the script from the top.
			return fmt.Errorf("%s failed on %d of %d (%s): %w\n"+
				"  the earlier ones already landed; re-run `gnopm publish` to see what is still missing",
				c.name, i+1, len(cmds), c.note, err)
		}
	}
	e.logf("\nran      %d command(s), all reported success\n", len(cmds))
	return nil
}

// stdFile hands a child process a real file descriptor. An Env built by a test
// writes to a buffer, which a subprocess cannot inherit; there is no
// subprocess in that case either, because startClient is replaced, so the
// fallback is only ever reached by a caller that built an Env by hand.
func stdFile(w any, fallback *os.File) *os.File {
	if f, ok := w.(*os.File); ok && f != nil {
		return f
	}
	return fallback
}
