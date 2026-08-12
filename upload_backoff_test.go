package profiler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// An internal test rather than one through the public API, because the public
// API cannot capture faster than once a minute — which is the very thing that
// makes this logic hard to reach from outside and easy to get wrong inside.

// pacingPlatform answers every upload with 429 and counts what reached it.
type pacingPlatform struct {
	server     *httptest.Server
	requests   atomic.Int64
	retryAfter string
}

func newPacingPlatform(t *testing.T, retryAfter string) *pacingPlatform {
	t.Helper()

	platform := &pacingPlatform{retryAfter: retryAfter}
	platform.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		platform.requests.Add(1)
		if platform.retryAfter != "" {
			w.Header().Set("Retry-After", platform.retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited"}}`))
	}))
	t.Cleanup(platform.server.Close)
	return platform
}

func newTestUploader(platform *pacingPlatform) *uploader {
	return newUploader(Config{
		Endpoint:      platform.server.URL,
		APIKey:        "test-key",
		HTTPClient:    platform.server.Client(),
		UploadTimeout: 5 * time.Second,
		QueueSize:     8,
	}, nopLogger{})
}

func job(profileType string) upload {
	return upload{
		metadata: Metadata{Service: "svc", ProfileType: profileType},
		payload:  []byte("not a real profile, and never parsed by this test"),
	}
}

// TestPacingIsHonouredRatherThanIgnored is the point of the change: a profiler
// configured to capture more often than its plan allows must stop sending, not
// post a payload every interval to have it refused.
func TestPacingIsHonouredRatherThanIgnored(t *testing.T) {
	platform := newPacingPlatform(t, "60")
	u := newTestUploader(platform)

	for range 5 {
		u.send(job("cpu"))
	}

	if got := platform.requests.Load(); got != 1 {
		t.Errorf("five captures produced %d requests; after the first 429 the rest should not be sent", got)
	}
}

// TestPacingIsPerProfileType: CPU and heap are different measurements on
// different schedules, and the server limits them separately. Holding all types
// back because one was paced would silently stop collecting the others.
func TestPacingIsPerProfileType(t *testing.T) {
	platform := newPacingPlatform(t, "60")
	u := newTestUploader(platform)

	u.send(job("cpu"))
	u.send(job("cpu"))
	u.send(job("heap"))

	if got := platform.requests.Load(); got != 2 {
		t.Errorf("got %d requests, want 2: the second cpu profile held back, the heap one sent", got)
	}
}

// TestPacingExpires: the hold is a delay, not a stop. A profiler that waits out
// the interval has to start sending again on its own.
func TestPacingExpires(t *testing.T) {
	platform := newPacingPlatform(t, "60")
	u := newTestUploader(platform)

	u.send(job("cpu"))
	if got := platform.requests.Load(); got != 1 {
		t.Fatalf("the first upload did not reach the platform")
	}

	// Wind the clock forward by expiring the hold rather than sleeping a minute.
	u.mu.Lock()
	u.notBefore["cpu"] = time.Now().Add(-time.Second)
	u.mu.Unlock()

	u.send(job("cpu"))
	if got := platform.requests.Load(); got != 2 {
		t.Errorf("got %d requests; the profiler never resumed after the interval passed", got)
	}
}

// TestAMissingRetryAfterStillPaces: a 429 without the header must not be read
// as "send again immediately", which is the reading that turns a limit into a
// loop.
func TestAMissingRetryAfterStillPaces(t *testing.T) {
	for _, header := range []string{"", "not-a-number", "0", "-5"} {
		platform := newPacingPlatform(t, header)
		u := newTestUploader(platform)

		u.send(job("cpu"))
		u.send(job("cpu"))

		if got := platform.requests.Load(); got != 1 {
			t.Errorf("Retry-After %q produced %d requests, want 1", header, got)
		}
	}
}
