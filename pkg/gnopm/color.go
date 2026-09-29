package gnopm

import (
	"io"
	"os"
	"strings"
)

// Colour, for the one report long enough that a reader has to scan it.
//
// `publish` is the only command that prints a list somebody reads rather than
// pipes: a hundred paths, most of them needing no decision, and three or four
// that do. Colour is what makes "which lines matter" a glance instead of a
// read. Everything else here stays plain, because a line you pipe is a line
// colour can only damage.
//
// Deliberately not a dependency, and deliberately six constants: SGR codes are
// thirty years old and stable, and a module for them would break the
// standard-library-only rule for less code than this comment.

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

// useColor reports whether w should receive SGR escapes.
func useColor(w io.Writer) bool { return colorDecision(isTerminal(w)) }

// colorDecision is the rule, separated from the question of whether this
// particular writer is a terminal so that it can be tested without one.
//
// The three rules everybody implements, in the order they win: CLICOLOR_FORCE
// says yes even into a pipe (a CI log renders them), NO_COLOR says no whatever
// else is true (no-color.org: present and not empty, whatever the value), and
// otherwise it is only for a terminal a human is watching. TERM=dumb is the
// terminal saying so itself.
func colorDecision(tty bool) bool {
	if v := os.Getenv("CLICOLOR_FORCE"); v != "" && v != "0" {
		return true
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return tty
}

// paint wraps s in the given codes, or returns it untouched when this Env is
// not writing to a terminal. Every caller stays one expression either way,
// which is what keeps the plain and coloured forms from drifting.
func (e *Env) paint(code, s string) string {
	if e == nil || !e.Color || code == "" || s == "" {
		return s
	}
	return code + s + ansiReset
}

// dim, bold and the rest are the named styles the report uses. Named rather
// than spelled at the call site so that a change of palette is one edit, and
// so that reading the report code says what a line means rather than what
// colour it is.
func (e *Env) dim(s string) string  { return e.paint(ansiDim, s) }
func (e *Env) bold(s string) string { return e.paint(ansiBold, s) }
func (e *Env) ok(s string) string   { return e.paint(ansiGreen, s) }
func (e *Env) warn(s string) string { return e.paint(ansiYellow, s) }
func (e *Env) bad(s string) string  { return e.paint(ansiRed, s) }

// label is the nine-column key every report line starts with, the same width
// `gnopm status` and the rest use. Dim, because it is the same six words on
// every run and the value beside it is not.
func (e *Env) label(s string) string {
	if s == "" {
		return "         "
	}
	return e.dim(pad(s, 9))
}

// pad right-pads to n runes, and never truncates: a label that outgrows the
// column pushes its line right rather than losing a character.
func pad(s string, n int) string {
	if d := n - len([]rune(s)); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// padLeft is pad's mirror, for the size column.
func padLeft(s string, n int) string {
	if d := n - len([]rune(s)); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}
