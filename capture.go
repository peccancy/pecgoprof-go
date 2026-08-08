package profiler

import (
	"bytes"
	"fmt"
	"runtime"
	"runtime/pprof"
	"time"
)

// CaptureCPU records a CPU profile for d and queues it for upload.
//
// It returns as soon as the profile is queued; the upload happens in the
// background. Only one CPU profile can run at a time in a Go process, so
// concurrent calls are serialised rather than failing.
//
// CPU profiling is never started automatically. It has a real cost, and
// switching it on is a decision the application owner makes explicitly.
func (p *Profiler) CaptureCPU(d time.Duration) error {
	if p == nil || !p.enabled {
		return nil
	}
	if d <= 0 {
		d = DefaultCPUDuration
	}

	p.cpuMu.Lock()
	defer p.cpuMu.Unlock()

	var buf bytes.Buffer
	if err := pprof.StartCPUProfile(&buf); err != nil {
		return fmt.Errorf("profiler: start CPU profile: %w", err)
	}

	// Stop early if the process is shutting down, so a 30 second capture
	// cannot hold up Stop for 30 seconds.
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-p.stop:
	}

	pprof.StopCPUProfile()
	return p.submit(TypeCPU, buf.Bytes())
}

// CaptureHeap records a heap profile: memory currently in use.
//
// It runs a garbage collection first so the profile reflects what is actually
// retained rather than what has not been collected yet. Without this a heap
// profile routinely looks like a leak that does not exist.
func (p *Profiler) CaptureHeap() error {
	if p == nil || !p.enabled {
		return nil
	}

	runtime.GC()
	return p.captureNamed(TypeHeap, "heap", 0)
}

// CaptureAllocs records an allocation profile: everything allocated since the
// process started, including memory already freed.
//
// This is the profile that answers "what is churning", as opposed to
// CaptureHeap's "what is retained".
func (p *Profiler) CaptureAllocs() error {
	if p == nil || !p.enabled {
		return nil
	}
	return p.captureNamed(TypeAllocs, "allocs", 0)
}

// CaptureGoroutines records a goroutine profile.
func (p *Profiler) CaptureGoroutines() error {
	if p == nil || !p.enabled {
		return nil
	}
	return p.captureNamed(TypeGoroutine, "goroutine", 0)
}

// CaptureMutex records a mutex contention profile.
//
// The runtime collects nothing unless mutex profiling is enabled, so the
// caller must have set runtime.SetMutexProfileFraction. EnableMutexProfiling
// does that.
func (p *Profiler) CaptureMutex() error {
	if p == nil || !p.enabled {
		return nil
	}
	if runtime.SetMutexProfileFraction(-1) == 0 {
		return fmt.Errorf("profiler: mutex profiling is disabled, call EnableMutexProfiling first")
	}
	return p.captureNamed(TypeMutex, "mutex", 0)
}

// CaptureBlock records a blocking profile.
//
// Like mutex profiling, this needs runtime.SetBlockProfileRate to have been
// called; EnableBlockProfiling does it.
func (p *Profiler) CaptureBlock() error {
	if p == nil || !p.enabled {
		return nil
	}
	return p.captureNamed(TypeBlock, "block", 0)
}

// captureNamed writes one of the runtime's named profiles.
func (p *Profiler) captureNamed(profileType, name string, debug int) error {
	profile := pprof.Lookup(name)
	if profile == nil {
		return fmt.Errorf("profiler: the runtime has no %q profile", name)
	}

	var buf bytes.Buffer
	// debug must be 0: any other value writes a human-readable dump instead of
	// the protobuf the platform parses.
	if err := profile.WriteTo(&buf, debug); err != nil {
		return fmt.Errorf("profiler: write %s profile: %w", name, err)
	}
	return p.submit(profileType, buf.Bytes())
}

// EnableMutexProfiling turns on the runtime's mutex profiler.
//
// rate is the reciprocal of the sampling fraction: 1 records every contention
// event, 100 records roughly one in a hundred. Start with a high number in
// production — sampling every event on a contended lock is itself expensive.
func EnableMutexProfiling(rate int) {
	if rate <= 0 {
		rate = 100
	}
	runtime.SetMutexProfileFraction(rate)
}

// DisableMutexProfiling turns the runtime's mutex profiler off.
func DisableMutexProfiling() { runtime.SetMutexProfileFraction(0) }

// EnableBlockProfiling turns on the runtime's block profiler.
//
// rate is in nanoseconds: an event blocking for longer than rate is recorded
// with probability 1. A rate of 10_000_000 (10ms) is a reasonable production
// starting point; 1 records everything and is expensive.
func EnableBlockProfiling(rateNanos int) {
	if rateNanos <= 0 {
		rateNanos = 10_000_000
	}
	runtime.SetBlockProfileRate(rateNanos)
}

// DisableBlockProfiling turns the runtime's block profiler off.
func DisableBlockProfiling() { runtime.SetBlockProfileRate(0) }

// Package-level shortcuts operating on the profiler created by Start. Each is
// a no-op when Start was never called or failed.

// CaptureCPU records a CPU profile with the default profiler.
func CaptureCPU(d time.Duration) error { return Default().CaptureCPU(d) }

// CaptureHeap records a heap profile with the default profiler.
func CaptureHeap() error { return Default().CaptureHeap() }

// CaptureAllocs records an allocation profile with the default profiler.
func CaptureAllocs() error { return Default().CaptureAllocs() }

// CaptureGoroutines records a goroutine profile with the default profiler.
func CaptureGoroutines() error { return Default().CaptureGoroutines() }

// CaptureMutex records a mutex profile with the default profiler.
func CaptureMutex() error { return Default().CaptureMutex() }

// CaptureBlock records a block profile with the default profiler.
func CaptureBlock() error { return Default().CaptureBlock() }
