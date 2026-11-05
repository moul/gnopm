package gnopm

import "errors"

// Exit codes, for the one case where "it failed" and "it did part of the
// work" are different answers to a caller.
//
// Everything in gnopm returns an error and main exits 1, which is right for
// every refusal: the command did nothing and the reason is on stderr. It is
// wrong for `publish -skip-blocked`, which deliberately publishes most of a
// workspace and leaves the rest out. A script that reads 1 there cannot tell
// a successful partial run from a run that never started, and the difference
// is whether anything was signed.
const (
	// ExitFailure is the default for any error: the command refused or
	// failed, and nothing it was asked to do happened.
	ExitFailure = 1

	// ExitPartial means the command did what it could and deliberately left
	// the rest out, naming every piece it left. Only `publish -skip-blocked`
	// returns it today.
	//
	// 3 rather than 2 because 2 is conventionally a usage error (Go's own
	// flag package exits 2 on a bad flag, and so do most CLIs), and a code
	// that means two things is a code nobody can branch on.
	ExitPartial = 3
)

// ExitError is an error carrying the code the process should exit with.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode is what the process should exit with for err: 0 for no error, an
// ExitError's own code when there is one anywhere in the chain, and
// ExitFailure otherwise.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return ExitFailure
}
