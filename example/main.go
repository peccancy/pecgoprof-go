// Command example is a small Go service wired up with the PecGoProf SDK.
//
// Run it against a local stack:
//
//	export PROFILER_API_KEY=pf_live_...
//	go run ./example
//
// It serves http://localhost:8090, burns some CPU and allocates on every
// request, and exposes endpoints that trigger a capture on demand.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	profiler "github.com/peccancy/pprof_analizator/sdk"
)

// stdLogger adapts the standard library's logger to the SDK's interface. Any
// logging library needs an adapter about this size.
type stdLogger struct{}

func (stdLogger) Debugf(format string, args ...any) { log.Printf("profiler: "+format, args...) }
func (stdLogger) Errorf(format string, args ...any) { log.Printf("profiler error: "+format, args...) }

func main() {
	p, err := profiler.Start(profiler.Config{
		APIKey:      os.Getenv("PROFILER_API_KEY"),
		Endpoint:    envOr("PROFILER_ENDPOINT", "http://localhost:8080"),
		Service:     envOr("PROFILER_SERVICE", "example-api"),
		Environment: envOr("PROFILER_ENVIRONMENT", "local"),
		Version:     "v0.1.0",
		GitSHA:      os.Getenv("GIT_SHA"),

		// A heap profile every ten minutes is cheap and gives the platform a
		// trend to compare against. CPU profiling stays on demand.
		HeapInterval: 10 * time.Minute,

		Logger: stdLogger{},
	})
	if err != nil {
		// Deliberately not fatal: the application runs fine without profiling.
		log.Printf("profiling is disabled: %v", err)
	}

	// Mutex and block profiles are empty unless the runtime is told to collect
	// them. Both sampling rates are conservative enough for production.
	profiler.EnableMutexProfiling(100)
	profiler.EnableBlockProfiling(10_000_000) // 10ms

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleWork)
	mux.HandleFunc("/capture/cpu", captureHandler("cpu", func() error {
		return p.CaptureCPU(15 * time.Second)
	}))
	mux.HandleFunc("/capture/heap", captureHandler("heap", p.CaptureHeap))
	mux.HandleFunc("/capture/allocs", captureHandler("allocs", p.CaptureAllocs))
	mux.HandleFunc("/capture/goroutines", captureHandler("goroutine", p.CaptureGoroutines))
	mux.HandleFunc("/capture/mutex", captureHandler("mutex", p.CaptureMutex))
	mux.HandleFunc("/capture/block", captureHandler("block", p.CaptureBlock))

	server := &http.Server{
		Addr:              ":8090",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("example service listening on %s (session %s)", server.Addr, p.SessionID())
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server: %v", err)
		}
	}()

	// Capture something immediately so the platform has data to show without
	// waiting for the heap interval.
	go func() {
		time.Sleep(2 * time.Second)
		if err := p.CaptureHeap(); err != nil {
			log.Printf("initial heap capture: %v", err)
		}
		if err := p.CaptureCPU(10 * time.Second); err != nil {
			log.Printf("initial CPU capture: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = server.Shutdown(shutdownCtx)

	// Give queued profiles a chance to land, but never hold shutdown hostage.
	if err := p.Stop(shutdownCtx); err != nil {
		log.Printf("profiler shutdown: %v", err)
	}
	log.Print("stopped")
}

// handleWork does something worth profiling: it allocates and serialises.
func handleWork(w http.ResponseWriter, r *http.Request) {
	orders := prepareOrders(200)

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(orders); err != nil {
		log.Printf("encode: %v", err)
	}
}

type order struct {
	ID       string   `json:"id"`
	Customer string   `json:"customer"`
	Items    []string `json:"items"`
	Total    float64  `json:"total"`
}

// prepareOrders allocates deliberately wastefully, so runtime.mallocgc shows up
// clearly in an allocation profile.
func prepareOrders(n int) []order {
	orders := make([]order, 0, n)

	for i := range n {
		items := []string{}
		for j := range 20 {
			items = append(items, fmt.Sprintf("item-%d-%d", i, j))
		}
		orders = append(orders, order{
			ID:       fmt.Sprintf("order-%d", i),
			Customer: strings.Repeat("customer", 4),
			Items:    items,
			Total:    float64(i) * 1.5,
		})
	}
	return orders
}

func captureHandler(name string, capture func() error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := capture(); err != nil {
			http.Error(w, fmt.Sprintf("%s capture failed: %v", name, err), http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintf(w, "%s profile queued for upload\n", name)
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
