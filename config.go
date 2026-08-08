// Package profiler collects Go pprof profiles and ships them to PecGoProf.
//
// The guiding rule of this package is that it must never be the reason an
// application fails. Start does no network I/O, every error is non-fatal, all
// uploads have timeouts, and a Profiler that failed to configure itself is
// simply inert rather than dangerous.
//
//	profiler.Start(profiler.Config{
//		APIKey:      os.Getenv("PROFILER_API_KEY"),
//		Endpoint:    "https://profiler.example.com",
//		Service:     "payment-api",
//		Environment: "production",
//		Version:     "v1.4.2",
//	})
//
//	profiler.CaptureCPU(30 * time.Second)
package profiler

import (
	"net/http"
	"time"
)

// Default tunables. All of them are deliberately conservative: this package
// runs inside somebody else's production process.
const (
	// DefaultUploadTimeout bounds one upload attempt.
	DefaultUploadTimeout = 30 * time.Second

	// DefaultQueueSize is how many pending uploads are buffered before new
	// ones are dropped. Dropping a profile is always better than blocking the
	// caller or growing without bound.
	DefaultQueueSize = 8

	// DefaultCPUDuration is used when CaptureCPU is called with a
	// non-positive duration.
	DefaultCPUDuration = 30 * time.Second

	// MinHeapInterval bounds periodic heap profiling. Anything faster is far
	// more likely to be a typo than an intention.
	MinHeapInterval = time.Minute
)

// Logger receives the SDK's diagnostics. Supply one to see why profiles are
// not arriving; leave it nil and the SDK stays silent.
//
// It is intentionally minimal so it can be satisfied by a two-line adapter
// over any logging library.
type Logger interface {
	Debugf(format string, args ...any)
	Errorf(format string, args ...any)
}

// Config configures a Profiler.
//
// Explicitly set fields always win over auto-detection. Anything left empty is
// filled in from the runtime and, where available, the Kubernetes downward
// API.
type Config struct {
	// APIKey is the project's ingest key (pf_live_…). Required.
	APIKey string

	// Endpoint is the platform's base URL, e.g. https://profiler.example.com.
	// Required. A trailing slash is fine.
	Endpoint string

	// Service names the logical application, e.g. "payment-api". Required.
	Service string

	// Environment is the deployment tier: production, staging, development,
	// local. Defaults to "local" — never inferred from the API key, because an
	// API key says which project you are, not where you run.
	Environment string

	// Version and GitSHA describe the running build. Optional, but they are
	// what makes "this regressed after v1.4.2" answerable.
	Version string
	GitSHA  string

	// InstanceID identifies this runtime. Leave empty to auto-detect: the
	// Kubernetes pod name when present, otherwise the hostname.
	InstanceID string

	// SessionID identifies this process execution. Leave empty and a fresh one
	// is generated at Start, which is almost always what you want — a new
	// session per process start is the point.
	SessionID string

	// HeapInterval enables periodic heap profiling. Zero disables it. Values
	// below MinHeapInterval are raised to it.
	//
	// There is deliberately no CPUInterval: continuous CPU profiling is not
	// something to switch on by accident.
	HeapInterval time.Duration

	// UploadTimeout bounds one upload attempt. Defaults to
	// DefaultUploadTimeout.
	UploadTimeout time.Duration

	// QueueSize is the pending-upload buffer. Defaults to DefaultQueueSize.
	QueueSize int

	// HTTPClient lets you supply a client with your own transport, proxy or
	// TLS configuration. The SDK sets a timeout on a client it creates itself
	// but never mutates one you provide.
	HTTPClient *http.Client

	// Logger receives diagnostics. Optional.
	Logger Logger

	// Tags are reserved for future labelled profiles. Ignored today.
	Tags map[string]string
}

// withDefaults returns a copy with empty fields filled in.
func (c Config) withDefaults() Config {
	if c.UploadTimeout <= 0 {
		c.UploadTimeout = DefaultUploadTimeout
	}
	if c.QueueSize <= 0 {
		c.QueueSize = DefaultQueueSize
	}
	if c.HeapInterval > 0 && c.HeapInterval < MinHeapInterval {
		c.HeapInterval = MinHeapInterval
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: c.UploadTimeout}
	}
	return c
}

// nopLogger is used when the caller supplies none.
type nopLogger struct{}

func (nopLogger) Debugf(string, ...any) {}
func (nopLogger) Errorf(string, ...any) {}
