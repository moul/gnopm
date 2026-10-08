package gnopm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A rate-limited node used to fail the whole query on the first 429. The
// existing chain tests use a fake that never limits, which is why none of them
// could see it.
func TestABCIQueryRetriesAThrottledNode(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"result":{"response":{"ResponseBase":{"Data":"aGk="}}}}`))
	}))
	defer srv.Close()
	var slept []time.Duration
	retrySleep = func(d time.Duration) { slept = append(slept, d) }
	defer func() { retrySleep = time.Sleep }()

	got, err := (&Chain{RPC: srv.URL}).ABCIQuery("vm/qfile", "x")
	if err != nil || got != "hi" {
		t.Fatalf("got %q, %v; want hi after two retries", got, err)
	}
	if atomic.LoadInt32(&calls) != 3 || len(slept) != 2 || slept[0] != time.Second {
		t.Fatalf("calls=%d slept=%v", calls, slept)
	}
}

func TestABCIQueryGivesUpOnAPermanentlyThrottledNode(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "no", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	retrySleep = func(time.Duration) {}
	defer func() { retrySleep = time.Sleep }()

	_, err := (&Chain{RPC: srv.URL}).ABCIQuery("vm/qfile", "x")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want a 429 error, got %v", err)
	}
	if atomic.LoadInt32(&calls) != maxQueryAttempts {
		t.Fatalf("calls=%d, want %d", calls, maxQueryAttempts)
	}
}
