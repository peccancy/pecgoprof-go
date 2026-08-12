package profiler

import (
	"testing"
	"time"
)

// TestEveryKindCanBeScheduled.
//
// The SDK used to schedule heap and nothing else, so a service that connected
// it reported memory and stayed silent about CPU, goroutines, mutexes and
// blocking — while the product's headline screen is a CPU flame graph. This is
// the test that says every kind can now be asked for.
func TestEveryKindCanBeScheduled(t *testing.T) {
	cfg := Config{
		APIKey:            "k",
		Endpoint:          "http://example.invalid",
		Service:           "svc",
		HeapInterval:      time.Minute,
		CPUInterval:       time.Minute,
		GoroutineInterval: time.Minute,
		AllocsInterval:    time.Minute,
		MutexInterval:     time.Minute,
		BlockInterval:     time.Minute,
	}.withDefaults()

	for name, interval := range map[string]time.Duration{
		"heap":      cfg.HeapInterval,
		"cpu":       cfg.CPUInterval,
		"goroutine": cfg.GoroutineInterval,
		"allocs":    cfg.AllocsInterval,
		"mutex":     cfg.MutexInterval,
		"block":     cfg.BlockInterval,
	} {
		if interval <= 0 {
			t.Errorf("%s cannot be scheduled", name)
		}
	}
}

// TestNothingRunsUnasked: every kind is off until an interval is named. A
// profiler that starts taking CPU profiles because somebody called Start is a
// profiler people switch off.
func TestNothingRunsUnasked(t *testing.T) {
	cfg := Config{APIKey: "k", Endpoint: "http://example.invalid", Service: "svc"}.withDefaults()

	for name, interval := range map[string]time.Duration{
		"heap":      cfg.HeapInterval,
		"cpu":       cfg.CPUInterval,
		"goroutine": cfg.GoroutineInterval,
		"allocs":    cfg.AllocsInterval,
		"mutex":     cfg.MutexInterval,
		"block":     cfg.BlockInterval,
	} {
		if interval != 0 {
			t.Errorf("%s runs at %s without being asked for", name, interval)
		}
	}
}

// TestATooEagerIntervalIsRaised: the platform refuses captures faster than a
// plan allows, so a client that insists only manufactures rejected uploads.
func TestATooEagerIntervalIsRaised(t *testing.T) {
	cfg := Config{
		APIKey: "k", Endpoint: "http://example.invalid", Service: "svc",
		HeapInterval: time.Second, CPUInterval: 2 * time.Second,
	}.withDefaults()

	if cfg.HeapInterval != MinCaptureInterval {
		t.Errorf("heap interval = %s, want it raised to %s", cfg.HeapInterval, MinCaptureInterval)
	}
	if cfg.CPUInterval != MinCaptureInterval {
		t.Errorf("cpu interval = %s, want it raised to %s", cfg.CPUInterval, MinCaptureInterval)
	}
}

// TestACPUCaptureCannotOverlapItself: a capture that runs longer than the gap
// to the next one would still be running when the next begins.
func TestACPUCaptureCannotOverlapItself(t *testing.T) {
	cfg := Config{
		APIKey: "k", Endpoint: "http://example.invalid", Service: "svc",
		CPUInterval: time.Minute, CPUDuration: 5 * time.Minute,
	}.withDefaults()

	if cfg.CPUDuration >= cfg.CPUInterval {
		t.Errorf("a %s capture every %s overlaps itself", cfg.CPUDuration, cfg.CPUInterval)
	}
}
