package gnopm

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A progress bar for the one thing in gnopm that is genuinely slow.
//
// Everything else here is local: a lock read, a git plumbing call, a file
// hash. Reading a chain about N packages is N round trips to a public RPC
// endpoint, and on a workspace the size of a real contracts repository that is
// a minute of a terminal that looks hung. A tool that looks hung gets killed,
// and a killed `publish` teaches people not to run it.
//
// Deliberately not a dependency. A bar is a carriage return, a count and a
// division; taking a module for it would break the standard-library-only rule
// for thirty lines of code.

// progressWidth is the bar itself, in cells. Narrow on purpose: the module
// path after it is the part that carries information.
const progressWidth = 20

// progressInterval throttles the redraw. Eight workers finishing 300 queries
// would otherwise write thousands of lines a second to a terminal that can
// show sixty.
const progressInterval = 60 * time.Millisecond

// progress draws a single self-rewriting line on stderr, and only where a
// human is watching it.
//
// Nothing here uses colour or a cursor escape: it rewrites its own line with a
// carriage return and pads the tail with spaces. That works on a terminal too
// old to have an opinion about ANSI, and it means NO_COLOR has nothing to
// disable.
type progress struct {
	mu      sync.Mutex
	w       io.Writer
	label   string
	enabled bool
	width   int
	lastLen int
	lastAt  time.Time
	drawn   bool
}

// newProgress returns a bar writing to e.Errw, silent unless that is a
// terminal a human is watching.
//
// Silent when piped, because the report that follows is the record and a
// half-overwritten bar in a log file is noise. `gnopm publish -print` pipes
// stdout and leaves stderr on the terminal, so the common case still gets it.
//
// Silent under -v too: a bar rewrites one line while the verbose trace scrolls
// past it, on the same stream, and the two together are less readable than
// either alone. -v already says what is happening, one line per answer.
func newProgress(e *Env, label string) *progress {
	p := &progress{w: e.Errw, label: label, width: terminalWidth()}
	p.enabled = !e.Quiet && !e.Verbose && isTerminal(e.Errw) && os.Getenv("TERM") != "dumb"
	return p
}

// step reports one more unit of work done. It matches the callback shape
// Probe.Warm wants, so the probe knows nothing about terminals and the bar
// knows nothing about chains.
func (p *progress) step(done, total int, detail string) {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if done < total && now.Sub(p.lastAt) < progressInterval {
		return
	}
	p.lastAt = now
	p.draw(done, total, detail)
}

// draw rewrites the line in place. Callers hold p.mu.
func (p *progress) draw(done, total int, detail string) {
	filled := 0
	if total > 0 {
		filled = done * progressWidth / total
	}
	if filled > progressWidth {
		filled = progressWidth
	}
	bar := strings.Repeat("\u2588", filled) + strings.Repeat("\u2591", progressWidth-filled)
	line := fmt.Sprintf("%s  [%s]  %d/%d", p.label, bar, done, total)
	if detail != "" {
		// The bar is drawn with box characters, one cell each, so counting
		// runes and not bytes is what keeps the truncation honest.
		room := p.width - len([]rune(line)) - 3
		if room > 8 {
			line += "  " + truncateLeft(detail, room)
		}
	}
	pad := ""
	if n := p.lastLen - len([]rune(line)); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	fmt.Fprintf(p.w, "\r%s%s", line, pad)
	p.lastLen = len([]rune(line))
	p.drawn = true
}

// stop erases the bar, leaving the line to whatever prints next. The bar is a
// progress indicator and not a result: once the work is done it has nothing
// left to say, and a finished bar left on screen is one more line between the
// reader and the report.
func (p *progress) stop() {
	if !p.enabled {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.drawn {
		return
	}
	fmt.Fprintf(p.w, "\r%s\r", strings.Repeat(" ", p.lastLen))
	p.drawn = false
	p.lastLen = 0
}

// isTerminal reports whether w is a character device, which is as close to
// "someone is looking at this" as the standard library gets. An io.Writer that
// is not an *os.File (a test buffer, a pipe) never is.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// terminalWidth reads COLUMNS and falls back to 80. Asking the kernel means
// an ioctl, which means syscall and a per-platform build; the bar only needs
// to know where to stop truncating, and being wrong costs a wrapped line that
// the next redraw replaces.
func terminalWidth() int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 20 {
		return n
	}
	return 80
}

// truncateLeft shortens a path from the front, because paths differ at the
// end: gno.land/p/moul/ulist/v1 and gno.land/p/moul/ulist/v2 share every
// character a right truncation would keep.
func truncateLeft(s string, max int) string {
	if max <= 1 || len(s) <= max {
		return s
	}
	return "…" + s[len(s)-(max-1):]
}
