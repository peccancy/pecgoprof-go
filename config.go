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

	// MinCaptureInterval bounds every periodic capture, heap included.
	MinCaptureInterval = time.Minute
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

	// The periodic captures. Every one is off until you give it an interval,
	// and none of them has a default.
	//
	// That is deliberate, and it is the reason CPU profiling has to be named
	// rather than inherited: a CPU profile stops the world briefly and costs
	// real time to take, so it is not something to switch on by accident. But
	// naming an interval is not an accident, and a profiler that can only ever
	// report heap is not a profiler — it is half of one, which is what this
	// used to be.
	//
	// Values below MinCaptureInterval are raised to it: the platform refuses
	// captures faster than a plan allows, and a client that insists is only
	// generating rejected uploads.
	HeapInterval      time.Duration
	CPUInterval       time.Duration
	GoroutineInterval time.Duration
	AllocsInterval    time.Duration
	MutexInterval     time.Duration
	BlockInterval     time.Duration

	// StopAfter turns the profiler off by itself, this long after Start.
	//
	// For the shape most people actually want: ship a build, watch it for a
	// couple of hours, and have profiling stop without a second deploy to turn
	// it off. Zero means never stop, which is right for a service being watched
	// for a leak — that shows up over days, not hours.
	//
	// What is already queued still uploads; only the capturing stops. Pair it
	// with the service's "stopped reporting" alert turned off in the platform,
	// or a profiler that did exactly what it was told will look like one that
	// broke.
	StopAfter time.Duration

	// CPUDuration is how long each CPU profile runs. Defaults to
	// DefaultCPUDuration. It is clamped below CPUInterval, because a capture
	// that outlives the gap to the next one would overlap itself.
	CPUDuration time.Duration

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
	for _, interval := range []*time.Duration{
		&c.HeapInterval, &c.CPUInterval, &c.GoroutineInterval,
		&c.AllocsInterval, &c.MutexInterval, &c.BlockInterval,
	} {
		if *interval > 0 && *interval < MinCaptureInterval {
			*interval = MinCaptureInterval
		}
	}

	if c.CPUDuration <= 0 {
		c.CPUDuration = DefaultCPUDuration
	}
	// A CPU capture that runs longer than the gap to the next one would start
	// again before it finished. Leave a margin rather than exactly meeting.
	if c.CPUInterval > 0 && c.CPUDuration >= c.CPUInterval {
		c.CPUDuration = c.CPUInterval / 2
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
