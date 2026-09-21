package gnopm

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// A bar is for a human watching a terminal. Anywhere else it is corruption in
// a log file, or worse, in data: Env.Errw is not always a terminal and the
// whole output discipline rests on knowing which is which.
func TestProgressIsSilentWhenNobodyIsWatching(t *testing.T) {
	for _, tc := range []struct {
		name    string
		quiet   bool
		verbose bool
	}{
		{name: "a buffer is not a terminal"},
		{name: "-q means quiet", quiet: true},
		// -v prints one line per answer on this same stream. A bar rewriting
		// its own line underneath a scrolling trace is less readable than
		// either alone, so -v takes the bar off.
		{name: "-v replaces the bar", verbose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			p := newProgress(&Env{Errw: &buf, Quiet: tc.quiet, Verbose: tc.verbose}, "reading test-1")
			p.step(1, 3, "gno.land/p/moul/md/v0")
			p.step(3, 3, "gno.land/p/moul/md/v2")
			p.stop()
			if buf.Len() != 0 {
				t.Fatalf("wrote %q to something no human is looking at", buf.String())
			}
		})
	}
}

// What the bar actually looks like, and that stop leaves the line empty for
// the report that follows. A finished bar left on screen is one more line
// between the reader and the answer.
func TestProgressDrawsAndThenErasesItself(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&Env{Errw: &buf}, "reading test-1")
	p.enabled = true // there is no terminal in a test; this is the only way in
	p.width = 80

	// done == total, which is the one step the throttle never swallows.
	p.step(4, 4, "gno.land/p/moul/md/v0")
	drawn := buf.String()
	for _, want := range []string{"\r", "reading test-1", "4/4", strings.Repeat("█", progressWidth), "gno.land/p/moul/md/v0"} {
		if !strings.Contains(drawn, want) {
			t.Fatalf("bar %q is missing %q", drawn, want)
		}
	}
	if strings.ContainsAny(drawn, "\n") {
		t.Fatalf("the bar wrote a newline, so it cannot rewrite its own line: %q", drawn)
	}

	buf.Reset()
	p.stop()
	erased := buf.String()
	if !strings.HasPrefix(erased, "\r") || !strings.HasSuffix(erased, "\r") {
		t.Fatalf("stop() did not return to the start of the line: %q", erased)
	}
	if strings.TrimSpace(strings.Trim(erased, "\r")) != "" {
		t.Fatalf("stop() left something on the line: %q", erased)
	}
	buf.Reset()
	p.stop()
	if buf.Len() != 0 {
		t.Fatalf("a second stop() wrote %q; it has nothing left to erase", buf.String())
	}
}

// A long path is truncated from the front, because two module paths differ at
// the end: a right truncation of gno.land/p/moul/ulist/v1 and .../v2 shows the
// same string twice.
func TestTruncateLeftKeepsTheEnd(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		max      int
	}{
		{"gno.land/p/moul/md/v0", "gno.land/p/moul/md/v0", 30},
		{"gno.land/p/moul/ulist/v1", "…/moul/ulist/v1", 15},
		{"short", "short", 5},
	} {
		if got := truncateLeft(tc.in, tc.max); got != tc.want {
			t.Errorf("truncateLeft(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

// COLUMNS is the only width the standard library can see without an ioctl, and
// a nonsense value must not make the bar wider than the terminal.
func TestTerminalWidthFallsBackTo80(t *testing.T) {
	for _, tc := range []struct {
		set  string
		want int
	}{{"", 80}, {"not a number", 80}, {"12", 80}, {"120", 120}} {
		t.Setenv("COLUMNS", tc.set)
		if tc.set == "" {
			os.Unsetenv("COLUMNS")
		}
		if got := terminalWidth(); got != tc.want {
			t.Errorf("terminalWidth() with COLUMNS=%q = %d, want %d", tc.set, got, tc.want)
		}
	}
}
