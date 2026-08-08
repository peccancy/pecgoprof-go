package profiler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Profile types the platform accepts.
const (
	TypeCPU       = "cpu"
	TypeHeap      = "heap"
	TypeAllocs    = "allocs"
	TypeGoroutine = "goroutine"
	TypeMutex     = "mutex"
	TypeBlock     = "block"
)

// Configuration errors. They are returned for diagnostics; a caller that
// ignores them gets an inert Profiler rather than a crash.
var (
	ErrNoAPIKey   = errors.New("profiler: APIKey is required")
	ErrNoEndpoint = errors.New("profiler: Endpoint is required")
	ErrNoService  = errors.New("profiler: Service is required")
)

// Profiler captures profiles and uploads them in the background.
//
// A nil *Profiler is usable: every method is a no-op on it. That is what makes
// `p, _ := profiler.Start(cfg)` safe even when the configuration was wrong.
type Profiler struct {
	config   Config
	metadata Metadata
	uploader *uploader
	logger   Logger

	// enabled is false when the configuration was rejected. The Profiler then
	// accepts calls and does nothing, so a misconfigured deployment loses its
	// profiles rather than its process.
	enabled bool

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup

	// cpuMu serialises CPU captures: the Go runtime allows only one CPU
	// profile at a time, and a second StartCPUProfile would fail.
	cpuMu sync.Mutex
}

var (
	globalMu sync.RWMutex
	global   *Profiler
)

// Start configures the profiler and begins any periodic collection.
//
// It performs no network I/O and never blocks: if the platform is unreachable,
// the application still starts and profiles are simply dropped. The returned
// error describes a configuration problem and is safe to ignore.
func Start(cfg Config) (*Profiler, error) {
	cfg = cfg.withDefaults()

	logger := cfg.Logger
	if logger == nil {
		logger = nopLogger{}
	}

	p := &Profiler{
		config: cfg,
		logger: logger,
		stop:   make(chan struct{}),
	}

	if err := validate(cfg); err != nil {
		logger.Errorf("profiler disabled: %v", err)
		setGlobal(p)
		return p, err
	}

	sessionID := strings.TrimSpace(cfg.SessionID)
	if sessionID == "" {
		sessionID = newSessionID()
	}

	p.metadata = detectMetadata(cfg, sessionID)
	p.uploader = newUploader(cfg, logger)
	p.enabled = true

	p.uploader.start(&p.wg)

	if cfg.HeapInterval > 0 {
		p.wg.Add(1)
		go p.heapLoop(cfg.HeapInterval)
	}

	logger.Debugf("profiler started: service=%s environment=%s instance=%s session=%s",
		p.metadata.Service, p.metadata.Environment, p.metadata.InstanceID, sessionID)

	setGlobal(p)
	return p, nil
}

func validate(cfg Config) error {
	switch {
	case strings.TrimSpace(cfg.APIKey) == "":
		return ErrNoAPIKey
	case strings.TrimSpace(cfg.Endpoint) == "":
		return ErrNoEndpoint
	case strings.TrimSpace(cfg.Service) == "":
		return ErrNoService
	default:
		return nil
	}
}

// Stop ends periodic collection and waits for queued uploads to finish or the
// context to expire. It is safe to call more than once.
func (p *Profiler) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}

	p.stopOnce.Do(func() {
		close(p.stop)
		if p.uploader != nil {
			p.uploader.close()
		}
	})

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Draining is best effort by design: shutdown must not be held up by
		// a profiling backend that stopped answering.
		return ctx.Err()
	}
}

// SessionID returns the identifier of this process execution.
func (p *Profiler) SessionID() string {
	if p == nil || !p.enabled {
		return ""
	}
	return p.metadata.SessionID
}

// InstanceID returns the identifier of this runtime.
func (p *Profiler) InstanceID() string {
	if p == nil || !p.enabled {
		return ""
	}
	return p.metadata.InstanceID
}

// Enabled reports whether the profiler is actually collecting.
func (p *Profiler) Enabled() bool { return p != nil && p.enabled }

// heapLoop captures a heap profile on a fixed interval.
func (p *Profiler) heapLoop(interval time.Duration) {
	defer p.wg.Done()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			if err := p.CaptureHeap(); err != nil {
				p.logger.Errorf("periodic heap capture failed: %v", err)
			}
		}
	}
}

// submit hands a captured profile to the uploader.
func (p *Profiler) submit(profileType string, payload []byte) error {
	if p == nil || !p.enabled {
		return nil
	}
	if len(payload) == 0 {
		return fmt.Errorf("profiler: %s capture produced no data", profileType)
	}

	metadata := p.metadata
	metadata.ProfileType = profileType

	return p.uploader.enqueue(upload{metadata: metadata, payload: payload})
}

func setGlobal(p *Profiler) {
	globalMu.Lock()
	defer globalMu.Unlock()
	global = p
}

// Default returns the Profiler created by the most recent Start, or nil.
// Methods on a nil *Profiler are no-ops, so the result needs no check.
func Default() *Profiler {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return global
}

// Stop stops the default profiler.
func Stop(ctx context.Context) error { return Default().Stop(ctx) }
