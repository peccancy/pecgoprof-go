# PecGoProf Go SDK

Collects Go pprof profiles and ships them to PecGoProf.

```bash
go get github.com/peccancy/pecgoprof-go
```

The package imports nothing outside the standard library. It is meant to sit
inside somebody else's production binary, and a profiler that drags in a
dependency tree is a profiler people refuse to adopt.

## Quick start

```go
package main

import (
    "context"
    "log"
    "os"
    "time"

    profiler "github.com/peccancy/pecgoprof-go"
)

func main() {
    p, err := profiler.Start(profiler.Config{
        APIKey:      os.Getenv("PROFILER_API_KEY"),
        Endpoint:    "https://pecgoprof.online",
        Service:     "payment-api",
        Environment: "production",
        Version:     "v1.4.2",
        GitSHA:      os.Getenv("GIT_SHA"),

        // Optional: a heap profile on a schedule. CPU profiling is never
        // automatic.
        HeapInterval: 10 * time.Minute,
    })
    if err != nil {
        // Not fatal: the service runs fine without profiling.
        log.Printf("profiling disabled: %v", err)
    }
    defer p.Stop(context.Background())

    // ... your application ...
}
```

That is the whole integration. `Start` does no network I/O, so it cannot delay
startup, and the error it returns describes a configuration mistake rather than
an unreachable server.

## Capturing profiles

```go
profiler.CaptureCPU(30 * time.Second) // blocks for the duration, uploads after
profiler.CaptureHeap()                // memory currently retained
profiler.CaptureAllocs()              // everything allocated since start
profiler.CaptureGoroutines()
profiler.CaptureMutex()               // needs EnableMutexProfiling first
profiler.CaptureBlock()               // needs EnableBlockProfiling first
```

Every one queues the upload and returns; the transfer happens on a background
goroutine. The package-level functions operate on the profiler created by
`Start`, or do nothing when it was never called.

`CaptureHeap` runs a garbage collection first, so the profile reflects what is
actually retained. Without that step a heap profile routinely looks like a leak
that does not exist.

Mutex and block profiles are empty unless the runtime is told to collect them:

```go
profiler.EnableMutexProfiling(100)     // sample roughly 1 in 100 contentions
profiler.EnableBlockProfiling(10_000_000) // record blocking over 10ms
```

Both sampling rates cost something. The defaults above are conservative enough
for production; `1` records everything and is not.

## What gets sent

Every upload carries metadata describing the process:

```json
{
  "service": "payment-api",
  "environment": "production",
  "instance_id": "payment-api-75f8497c7b-jkd2x",
  "session_id": "019abc…",
  "profile_type": "cpu",
  "runtime":        { "go_version": "go1.26.4", "os": "linux", "arch": "amd64", "pid": 1, "hostname": "…" },
  "deployment":     { "version": "v1.4.2", "git_sha": "…" },
  "infrastructure": { "provider": "kubernetes", "cluster": "…", "namespace": "…", "pod": "…" }
}
```

Anything you do not set is detected:

| Field | Detected from |
| --- | --- |
| `instance_id` | Kubernetes pod name, falling back to the hostname |
| `session_id` | Generated at `Start` — a new one per process execution |
| `runtime.*` | `runtime.Version`, `GOOS`, `GOARCH`, `os.Getpid`, `os.Hostname` |
| `infrastructure.*` | The downward API and the service account mount, when running in a pod |

**Explicit configuration always wins.** Detection only fills in what you left
empty.

Note what the SDK does *not* send: a project ID. The project is established by
the API key, and a client-supplied one would be ignored — or rather, rejected.

### instance_id and session_id

`instance_id` says *where* the process runs; `session_id` says *which run* it
is. A pod that restarts keeps its instance and gets a new session, which is what
makes "this instance restarts every twenty minutes" visible in the UI.

`instance_id` deliberately falls back to `"unknown"` rather than to a random
value: a random one would make every restart look like a brand new machine.

## Failure behaviour

The SDK is built so that it can never be the reason an application fails:

- `Start` performs no network I/O and returns immediately.
- A rejected configuration produces an inert profiler, not a crash. Every method
  on it is a no-op — including on a `nil` receiver, so `p, _ := Start(cfg)`
  followed by `p.CaptureHeap()` is safe.
- Uploads have timeouts and are never retried. A dropped profile costs a data
  point; a retry storm against a struggling backend costs an incident.
- A full queue drops the profile and logs it, rather than blocking the caller.
- `Stop(ctx)` drains what is queued but gives up when the context expires, so
  shutdown is never held hostage.

Nothing is logged unless you supply a `Logger`:

```go
type stdLogger struct{}

func (stdLogger) Debugf(f string, a ...any) { log.Printf("profiler: "+f, a...) }
func (stdLogger) Errorf(f string, a ...any) { log.Printf("profiler error: "+f, a...) }
```

## Configuration

| Field | Default | Purpose |
| --- | --- | --- |
| `APIKey` | — | **Required.** The project's `pf_live_…` key |
| `Endpoint` | — | **Required.** Platform base URL |
| `Service` | — | **Required.** Logical application name |
| `Environment` | `local` | Deployment tier. Never inferred from the API key |
| `Version`, `GitSHA` | — | The running build |
| `InstanceID`, `SessionID` | detected | Override the detection |
| `HeapInterval` | off | Periodic heap profiling; values under a minute are raised |
| `UploadTimeout` | 30s | Bounds one upload |
| `QueueSize` | 8 | Pending uploads before new ones are dropped |
| `HTTPClient` | — | Supply your own transport, proxy or TLS config |
| `Logger` | none | Diagnostics |

## Shutting down

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
profiler.Stop(ctx)
```

Safe to call more than once.

## Example

`example/` is a small HTTP service wired up with the SDK, with endpoints that
trigger a capture on demand:

```bash
export PROFILER_API_KEY=pf_live_...
go run ./example
curl localhost:8090/capture/cpu
```
