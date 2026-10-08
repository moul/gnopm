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
	spacing := throttledSpacing
	throttledSpacing, paceSpacing = 0, 0
	defer func() { retrySleep = time.Sleep; throttledSpacing = spacing; paceSpacing = 0 }()

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
	spacing := throttledSpacing
	throttledSpacing, paceSpacing = 0, 0
	defer func() { retrySleep = time.Sleep; throttledSpacing = spacing; paceSpacing = 0 }()

	_, err := (&Chain{RPC: srv.URL}).ABCIQuery("vm/qfile", "x")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("want a 429 error, got %v", err)
	}
	if atomic.LoadInt32(&calls) != maxThrottledAttempts {
		t.Fatalf("calls=%d, want %d", calls, maxThrottledAttempts)
	}
}

// After the first 429 the client spaces its own queries, so a scan stops
// asking at the rate that tripped the limit instead of burning retries on it.
func TestABCIQuerySpacesItselfAfterBeingThrottled(t *testing.T) {
	var first int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&first, 1) == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"result":{"response":{"ResponseBase":{"Data":"aGk="}}}}`))
	}))
	defer srv.Close()
	var slept []time.Duration
	retrySleep = func(d time.Duration) { slept = append(slept, d) }
	paceSpacing, paceLast = 0, time.Time{}
	defer func() { retrySleep = time.Sleep; paceSpacing, paceLast = 0, time.Time{} }()

	c := &Chain{RPC: srv.URL}
	for i := 0; i < 3; i++ {
		if _, err := c.ABCIQuery("vm/qfile", "x"); err != nil {
			t.Fatal(err)
		}
	}
	if paceSpacing != throttledSpacing {
		t.Fatalf("spacing %v, want %v", paceSpacing, throttledSpacing)
	}
	// one backoff for the 429, then at least the two queries that followed
	// it were held to the spacing
	if len(slept) < 3 {
		t.Fatalf("slept %v: queries after a 429 were not spaced", slept)
	}
}

// The regression #87 left behind: six tries at 1s, 2s, 4s, 8s, 16s is 31s of
// patience, and a tripped https://rpc.gno.land refuses for 66s. A node that
// keeps saying 429 for longer than the schedule is indistinguishable from one
// that never relents, so `publish -republish` over a whole repo still died
// with nothing reported. The two tests above cannot see it: one relents on the
// third call, the other never relents at all.
func TestABCIQueryOutlastsARateLimitWindow(t *testing.T) {
	// How long the fake endpoint refuses, in the simulated clock the stubbed
	// retrySleep advances: longer than the old 31s budget, and as long as the
	// window measured on the real one.
	const window = 66 * time.Second

	var (
		calls   int32
		elapsed time.Duration
		slept   []time.Duration
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if elapsed < window {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"result":{"response":{"ResponseBase":{"Data":"aGk="}}}}`))
	}))
	defer srv.Close()
	retrySleep = func(d time.Duration) { slept = append(slept, d); elapsed += d }
	spacing := throttledSpacing
	throttledSpacing, paceSpacing = 0, 0
	defer func() { retrySleep = time.Sleep; throttledSpacing = spacing; paceSpacing = 0 }()

	got, err := (&Chain{RPC: srv.URL}).ABCIQuery("vm/qfile", "x")
	if err != nil || got != "hi" {
		t.Fatalf("got %q, %v; want hi once the window passed, slept=%v", got, err, slept)
	}
	var total time.Duration
	for _, d := range slept {
		total += d
		if d > maxQueryDelay {
			t.Fatalf("one wait of %v, want none above the %v cap", d, maxQueryDelay)
		}
	}
	if total < window {
		t.Fatalf("waited %v in total, want more than the %v window", total, window)
	}
}

// Spacing is what keeps the scan from re-tripping the limit, so the number has
// to stay under what the endpoint sustains: about 90 queries a minute, 2026-10-08.
func TestThrottledSpacingStaysUnderTheMeasuredCeiling(t *testing.T) {
	const ceiling = 90.0 // queries a minute
	if rate := float64(time.Minute) / float64(throttledSpacing); rate > ceiling {
		t.Fatalf("spacing %v asks for %.0f queries a minute, over the %.0f the endpoint sustains",
			throttledSpacing, rate, ceiling)
	}
}

// A node that is simply down must be reported as down, fast. Retrying a 502 on
// the throttled budget would hold every caller for two minutes before saying
// so, and `Warm` fans out one of these per package: the whole suite paid 1m7s
// for this one fake before the budgets were split.
func TestABCIQueryGivesUpQuicklyOnAnUnavailableNode(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "gateway is having a day", http.StatusBadGateway)
	}))
	defer srv.Close()
	var slept []time.Duration
	retrySleep = func(d time.Duration) { slept = append(slept, d) }
	spacing := throttledSpacing
	throttledSpacing, paceSpacing = 0, 0
	defer func() { retrySleep = time.Sleep; throttledSpacing = spacing; paceSpacing = 0 }()

	_, err := (&Chain{RPC: srv.URL}).ABCIQuery("vm/qfile", "x")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("want a 502 error, got %v", err)
	}
	if atomic.LoadInt32(&calls) != maxUnavailableAttempts {
		t.Fatalf("calls=%d, want %d", calls, maxUnavailableAttempts)
	}
	var total time.Duration
	for _, d := range slept {
		total += d
	}
	if total > 5*time.Second {
		t.Fatalf("waited %v on a node that is down, want it reported promptly", total)
	}
}
