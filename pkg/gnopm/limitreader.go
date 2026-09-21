package gnopm

import "io"

// newLimitReader bounds how much of a remote response is read into memory.
func newLimitReader(r io.Reader, n int64) io.Reader { return io.LimitReader(r, n) }
