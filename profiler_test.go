package profiler_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pprof "github.com/google/pprof/profile"

	profiler "github.com/peccancy/pecgoprof-go"
)

// received is one upload the fake platform accepted.
type received struct {
	metadata profiler.Metadata
	payload  []byte
	apiKey   string
}

// fakePlatform stands in for the ingest API.
type fakePlatform struct {
	server *httptest.Server

	mu      sync.Mutex
	uploads []received
	status  int
	block   chan struct{}
}

func newFakePlatform(t *testing.T) *fakePlatform {
	t.Helper()

	platform := &fakePlatform{status: http.StatusCreated}
	platform.server = httptest.NewServer(http.HandlerFunc(platform.handle))
	t.Cleanup(platform.server.Close)
	return platform
}

func (f *fakePlatform) handle(w http.ResponseWriter, r *http.Request) {
	if f.block != nil {
		<-f.block
	}

	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	entry := received{apiKey: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}
	reader := multipart.NewReader(r.Body, params["boundary"])

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		switch part.FormName() {
		case "metadata":
			if err := json.NewDecoder(part).Decode(&entry.metadata); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		case "profile":
			entry.payload, _ = io.ReadAll(part)
		}
		_ = part.Close()
	}

	f.mu.Lock()
	f.uploads = append(f.uploads, entry)
	status := f.status
	f.mu.Unlock()

	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"profile":{"id":"00000000-0000-4000-8000-000000000000"}}`))
}

// waitForUploads polls until n uploads have arrived or the deadline passes.
func (f *fakePlatform) waitForUploads(t *testing.T, n int) []received {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		count := len(f.uploads)
		f.mu.Unlock()

		if count >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	uploads := make([]received, len(f.uploads))
	copy(uploads, f.uploads)
	if len(uploads) < n {
		t.Fatalf("received %d uploads, want %d", len(uploads), n)
	}
	return uploads
}

// testLogger captures diagnostics so a test can assert on them.
type testLogger struct {
	mu     sync.Mutex
	errors []string
}

func (l *testLogger) Debugf(string, ...any) {}

func (l *testLogger) Errorf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, format)
}

func (l *testLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.errors)
}

func startProfiler(t *testing.T, platform *fakePlatform, mutate func(*profiler.Config)) *profiler.Profiler {
	t.Helper()

	cfg := profiler.Config{
		APIKey:      "pf_live_test_key",
		Endpoint:    platform.server.URL,
		Service:     "payment-api",
		Environment: "production",
		Version:     "v1.4.2",
		GitSHA:      "abc123",
		InstanceID:  "test-instance",
	}
	if mutate != nil {
		mutate(&cfg)
	}

	p, err := profiler.Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})
	return p
}

func TestCaptureHeapUploadsAValidProfile(t *testing.T) {
	platform := newFakePlatform(t)
	p := startProfiler(t, platform, nil)

	if err := p.CaptureHeap(); err != nil {
		t.Fatalf("CaptureHeap: %v", err)
	}

	uploads := platform.waitForUploads(t, 1)
	upload := uploads[0]

	if upload.apiKey != "pf_live_test_key" {
		t.Errorf("api key = %q, want the configured one", upload.apiKey)
	}
	if upload.metadata.ProfileType != profiler.TypeHeap {
		t.Errorf("profile_type = %q, want heap", upload.metadata.ProfileType)
	}
	if upload.metadata.Service != "payment-api" {
		t.Errorf("service = %q, want payment-api", upload.metadata.Service)
	}

	// The payload has to be something the platform can actually parse.
	if _, err := pprof.ParseData(upload.payload); err != nil {
		t.Errorf("uploaded payload is not a valid pprof profile: %v", err)
	}
}

func TestMetadataIsAutoDetected(t *testing.T) {
	platform := newFakePlatform(t)
	p := startProfiler(t, platform, func(cfg *profiler.Config) {
		cfg.InstanceID = "" // force detection
	})

	if err := p.CaptureGoroutines(); err != nil {
		t.Fatalf("CaptureGoroutines: %v", err)
	}

	metadata := platform.waitForUploads(t, 1)[0].metadata

	if metadata.Runtime.GoVersion == "" {
		t.Error("go_version was not detected")
	}
	if metadata.Runtime.OS == "" || metadata.Runtime.Arch == "" {
		t.Errorf("os/arch were not detected: %+v", metadata.Runtime)
	}
	if metadata.Runtime.PID == 0 {
		t.Error("pid was not detected")
	}
	if metadata.InstanceID == "" {
		t.Error("instance_id was not detected")
	}
	if metadata.SessionID == "" {
		t.Error("session_id was not generated")
	}
	if metadata.Deployment.Version != "v1.4.2" || metadata.Deployment.GitSHA != "abc123" {
		t.Errorf("deployment metadata = %+v, want the configured values", metadata.Deployment)
	}
}

// TestExplicitConfigurationWins is the rule from the spec: what the caller
// states must never be overwritten by detection.
func TestExplicitConfigurationWins(t *testing.T) {
	platform := newFakePlatform(t)
	p := startProfiler(t, platform, func(cfg *profiler.Config) {
		cfg.InstanceID = "explicit-instance"
		cfg.SessionID = "explicit-session"
		cfg.Environment = "staging"
	})

	if err := p.CaptureGoroutines(); err != nil {
		t.Fatalf("CaptureGoroutines: %v", err)
	}

	metadata := platform.waitForUploads(t, 1)[0].metadata

	if metadata.InstanceID != "explicit-instance" {
		t.Errorf("instance_id = %q, want the explicit value", metadata.InstanceID)
	}
	if metadata.SessionID != "explicit-session" {
		t.Errorf("session_id = %q, want the explicit value", metadata.SessionID)
	}
	if metadata.Environment != "staging" {
		t.Errorf("environment = %q, want staging", metadata.Environment)
	}
}

func TestEnvironmentDefaultsToLocal(t *testing.T) {
	platform := newFakePlatform(t)
	p := startProfiler(t, platform, func(cfg *profiler.Config) { cfg.Environment = "" })

	if err := p.CaptureGoroutines(); err != nil {
		t.Fatalf("CaptureGoroutines: %v", err)
	}

	if env := platform.waitForUploads(t, 1)[0].metadata.Environment; env != "local" {
		t.Errorf("environment = %q, want local", env)
	}
}

func TestSessionIDIsFreshPerProfiler(t *testing.T) {
	platform := newFakePlatform(t)

	first := startProfiler(t, platform, nil)
	second := startProfiler(t, platform, nil)

	if first.SessionID() == "" || second.SessionID() == "" {
		t.Fatal("a profiler has no session id")
	}
	// Two process executions must be distinguishable; two Starts stand in for
	// two processes here.
	if first.SessionID() == second.SessionID() {
		t.Error("two profilers share a session id")
	}
}

// TestStartWithBadConfigIsInert covers the central promise: a misconfigured
// profiler costs profiles, never the process.
func TestStartWithBadConfigIsInert(t *testing.T) {
	tests := []struct {
		name    string
		config  profiler.Config
		wantErr error
	}{
		{
			name:    "no api key",
			config:  profiler.Config{Endpoint: "http://localhost", Service: "svc"},
			wantErr: profiler.ErrNoAPIKey,
		},
		{
			name:    "no endpoint",
			config:  profiler.Config{APIKey: "k", Service: "svc"},
			wantErr: profiler.ErrNoEndpoint,
		},
		{
			name:    "no service",
			config:  profiler.Config{APIKey: "k", Endpoint: "http://localhost"},
			wantErr: profiler.ErrNoService,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := profiler.Start(tt.config)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if p == nil {
				t.Fatal("Start returned a nil profiler; callers ignoring the error would panic")
			}
			if p.Enabled() {
				t.Error("a rejected configuration produced an enabled profiler")
			}

			// Every operation must be a harmless no-op.
			if err := p.CaptureHeap(); err != nil {
				t.Errorf("CaptureHeap on a disabled profiler: %v", err)
			}
			if err := p.CaptureCPU(time.Millisecond); err != nil {
				t.Errorf("CaptureCPU on a disabled profiler: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := p.Stop(ctx); err != nil {
				t.Errorf("Stop on a disabled profiler: %v", err)
			}
		})
	}
}

// TestNilProfilerIsSafe: `p, _ := Start(...)` then using p must never panic,
// and neither must the package-level shortcuts before Start is called.
func TestNilProfilerIsSafe(t *testing.T) {
	var p *profiler.Profiler

	if p.Enabled() {
		t.Error("a nil profiler reports itself as enabled")
	}
	if p.SessionID() != "" || p.InstanceID() != "" {
		t.Error("a nil profiler returned identifiers")
	}

	for name, capture := range map[string]func() error{
		"heap":       p.CaptureHeap,
		"allocs":     p.CaptureAllocs,
		"goroutines": p.CaptureGoroutines,
		"mutex":      p.CaptureMutex,
		"block":      p.CaptureBlock,
	} {
		if err := capture(); err != nil {
			t.Errorf("%s on a nil profiler: %v", name, err)
		}
	}

	if err := p.CaptureCPU(time.Millisecond); err != nil {
		t.Errorf("CaptureCPU on a nil profiler: %v", err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Errorf("Stop on a nil profiler: %v", err)
	}
}

// TestStartDoesNotBlockWhenPlatformIsDown is the requirement that the SDK must
// never delay application startup.
func TestStartDoesNotBlockWhenPlatformIsDown(t *testing.T) {
	logger := &testLogger{}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		p, err := profiler.Start(profiler.Config{
			APIKey: "pf_live_key",
			// A port nothing listens on.
			Endpoint:      "http://127.0.0.1:1",
			Service:       "payment-api",
			UploadTimeout: 100 * time.Millisecond,
			Logger:        logger,
		})
		if err != nil {
			t.Errorf("Start: %v", err)
		}
		done <- time.Since(start)

		_ = p.CaptureGoroutines()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	}()

	select {
	case elapsed := <-done:
		// Start performs no network I/O at all, so this is generous.
		if elapsed > time.Second {
			t.Errorf("Start took %s with an unreachable platform, want it to return immediately", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start blocked when the platform was unreachable")
	}
}

// TestUploadFailuresAreNonFatal: a rejecting backend must produce log lines,
// not errors surfacing into the application.
func TestUploadFailuresAreNonFatal(t *testing.T) {
	platform := newFakePlatform(t)
	platform.mu.Lock()
	platform.status = http.StatusInternalServerError
	platform.mu.Unlock()

	logger := &testLogger{}
	p := startProfiler(t, platform, func(cfg *profiler.Config) { cfg.Logger = logger })

	// Queuing succeeds; the failure happens in the background.
	if err := p.CaptureGoroutines(); err != nil {
		t.Errorf("CaptureGoroutines returned an error for a backend failure: %v", err)
	}

	platform.waitForUploads(t, 1)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && logger.count() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if logger.count() == 0 {
		t.Error("a rejected upload produced no diagnostic")
	}
}

// TestQueueFullDropsRatherThanBlocks: back pressure must never reach the
// application.
func TestQueueFullDropsRatherThanBlocks(t *testing.T) {
	platform := newFakePlatform(t)
	platform.block = make(chan struct{})

	p := startProfiler(t, platform, func(cfg *profiler.Config) {
		cfg.QueueSize = 1
		cfg.UploadTimeout = 500 * time.Millisecond
	})

	// The first capture occupies the worker, the second fills the queue, and
	// the rest have nowhere to go.
	var dropped int
	captured := make(chan struct{})

	go func() {
		defer close(captured)
		for range 10 {
			if errors.Is(p.CaptureGoroutines(), profiler.ErrQueueFull) {
				dropped++
			}
		}
	}()

	select {
	case <-captured:
	case <-time.After(5 * time.Second):
		close(platform.block)
		t.Fatal("captures blocked while the platform was stalled")
	}

	close(platform.block)

	if dropped == 0 {
		t.Error("no profile was dropped although the queue was full and the backend stalled")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	platform := newFakePlatform(t)
	p := startProfiler(t, platform, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := range 3 {
		if err := p.Stop(ctx); err != nil {
			t.Errorf("Stop call %d: %v", i+1, err)
		}
	}
}

func TestCaptureCPUProducesACPUProfile(t *testing.T) {
	platform := newFakePlatform(t)
	p := startProfiler(t, platform, nil)

	if err := p.CaptureCPU(150 * time.Millisecond); err != nil {
		t.Fatalf("CaptureCPU: %v", err)
	}

	upload := platform.waitForUploads(t, 1)[0]
	if upload.metadata.ProfileType != profiler.TypeCPU {
		t.Errorf("profile_type = %q, want cpu", upload.metadata.ProfileType)
	}

	parsed, err := pprof.ParseData(upload.payload)
	if err != nil {
		t.Fatalf("uploaded CPU profile does not parse: %v", err)
	}
	// A CPU profile must declare cpu/nanoseconds, otherwise the platform will
	// read the wrong column.
	var hasCPU bool
	for _, sampleType := range parsed.SampleType {
		if sampleType.Type == "cpu" {
			hasCPU = true
		}
	}
	if !hasCPU {
		t.Errorf("sample types = %v, want one of them to be cpu", parsed.SampleType)
	}
}

// TestConcurrentCPUCapturesAreSerialised: the runtime permits only one CPU
// profile at a time, so overlapping calls must queue rather than fail.
func TestConcurrentCPUCapturesAreSerialised(t *testing.T) {
	platform := newFakePlatform(t)
	p := startProfiler(t, platform, func(cfg *profiler.Config) { cfg.QueueSize = 4 })

	var wg sync.WaitGroup
	errs := make(chan error, 2)

	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.CaptureCPU(100 * time.Millisecond); err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent CaptureCPU: %v", err)
	}
	platform.waitForUploads(t, 2)
}

func TestMutexCaptureRequiresEnabling(t *testing.T) {
	platform := newFakePlatform(t)
	p := startProfiler(t, platform, nil)

	profiler.DisableMutexProfiling()
	if err := p.CaptureMutex(); err == nil {
		t.Error("CaptureMutex succeeded although mutex profiling is off")
	}

	profiler.EnableMutexProfiling(100)
	t.Cleanup(profiler.DisableMutexProfiling)

	if err := p.CaptureMutex(); err != nil {
		t.Errorf("CaptureMutex after enabling: %v", err)
	}
	if metadata := platform.waitForUploads(t, 1)[0].metadata; metadata.ProfileType != profiler.TypeMutex {
		t.Errorf("profile_type = %q, want mutex", metadata.ProfileType)
	}
}

func TestPeriodicHeapProfiling(t *testing.T) {
	platform := newFakePlatform(t)

	// The configured interval is below the minimum, so it is raised rather
	// than honoured — this asserts the clamp exists.
	p := startProfiler(t, platform, func(cfg *profiler.Config) {
		cfg.HeapInterval = time.Millisecond
	})

	if !p.Enabled() {
		t.Fatal("profiler is not enabled")
	}

	// With the clamp in place no periodic upload can have happened yet.
	time.Sleep(100 * time.Millisecond)

	platform.mu.Lock()
	count := len(platform.uploads)
	platform.mu.Unlock()

	if count > 0 {
		t.Errorf("received %d periodic uploads within 100ms; the minimum interval was not applied", count)
	}
}

func TestDefaultProfilerShortcuts(t *testing.T) {
	platform := newFakePlatform(t)
	startProfiler(t, platform, nil)

	if err := profiler.CaptureGoroutines(); err != nil {
		t.Fatalf("package-level CaptureGoroutines: %v", err)
	}
	if profiler.Default() == nil {
		t.Fatal("Default returned nil after Start")
	}

	if metadata := platform.waitForUploads(t, 1)[0].metadata; metadata.ProfileType != profiler.TypeGoroutine {
		t.Errorf("profile_type = %q, want goroutine", metadata.ProfileType)
	}
}
