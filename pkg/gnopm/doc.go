// Package gnopm is the operations behind the gnopm command: Sync, Bump,
// Unbump, Verify, Tidy, Deversion, Clean, Publish, Graph and the rest.
//
// It is a package rather than a pile of code under main so that other programs
// can drive a gno workspace without shelling out to a binary and parsing its
// output. The CLI in the repository root is one caller; the operations here do
// not assume they have a terminal, and every one takes an Env saying where the
// workspace is and where its two output streams go.
//
// The split of those streams is load-bearing and not a detail: data goes to
// Env.Out and nothing else ever does, so `gnopm ls -q | xargs ...` receives
// module paths and not progress lines, while diagnostics, warnings and
// progress go to Env.Errw where a human sees them and a pipe does not.
//
// The gnomod.lock format lives next door in moul.io/gnopm/pkg/gnomodlock, with
// no dependency on this package or on any repository layout, because a lock
// format is only a format if more than one program can read it.
//
// Standard library only, on purpose. See CONTRIBUTING.md for what that costs
// and what it buys.
package gnopm
