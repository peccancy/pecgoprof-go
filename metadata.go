package profiler

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"runtime"
	"strings"
)

// Metadata is what accompanies every uploaded profile. Its JSON shape is the
// ingest API's contract.
type Metadata struct {
	Service     string `json:"service"`
	Environment string `json:"environment"`
	InstanceID  string `json:"instance_id"`
	SessionID   string `json:"session_id"`
	ProfileType string `json:"profile_type"`

	Runtime        RuntimeInfo        `json:"runtime"`
	Deployment     DeploymentInfo     `json:"deployment"`
	Infrastructure InfrastructureInfo `json:"infrastructure"`
}

// RuntimeInfo describes the Go process.
type RuntimeInfo struct {
	GoVersion string `json:"go_version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	PID       int    `json:"pid"`
	Hostname  string `json:"hostname"`
}

// DeploymentInfo describes the build.
type DeploymentInfo struct {
	Version string `json:"version"`
	GitSHA  string `json:"git_sha"`
}

// InfrastructureInfo describes where the process runs.
type InfrastructureInfo struct {
	Provider  string `json:"provider"`
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
}

// DefaultEnvironment is used when the caller names none.
//
// Defaulting to "local" rather than guessing production is deliberate: a wrong
// environment label is worse than an honest one, and the API key cannot tell
// us where the process runs.
const DefaultEnvironment = "local"

// detectMetadata assembles the metadata template for a session. Explicit
// configuration always wins; only empty fields are filled in.
func detectMetadata(cfg Config, sessionID string) Metadata {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}

	infra := detectInfrastructure()

	environment := strings.TrimSpace(cfg.Environment)
	if environment == "" {
		environment = DefaultEnvironment
	}

	instanceID := strings.TrimSpace(cfg.InstanceID)
	if instanceID == "" {
		instanceID = detectInstanceID(infra, hostname)
	}

	return Metadata{
		Service:     strings.TrimSpace(cfg.Service),
		Environment: environment,
		InstanceID:  instanceID,
		SessionID:   sessionID,
		Runtime: RuntimeInfo{
			GoVersion: runtime.Version(),
			OS:        runtime.GOOS,
			Arch:      runtime.GOARCH,
			PID:       os.Getpid(),
			Hostname:  hostname,
		},
		Deployment: DeploymentInfo{
			Version: strings.TrimSpace(cfg.Version),
			GitSHA:  strings.TrimSpace(cfg.GitSHA),
		},
		Infrastructure: infra,
	}
}

// detectInstanceID picks the most stable identifier available.
//
// A pod name survives a container restart and is what an operator sees in
// kubectl, so it beats the hostname even though the two usually match. The
// fallback chain ends at "unknown" rather than a random value: a random
// instance ID would make every restart look like a new machine.
func detectInstanceID(infra InfrastructureInfo, hostname string) string {
	for _, candidate := range []string{infra.Pod, hostname} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return candidate
		}
	}
	return "unknown"
}

// Environment variables the Kubernetes downward API conventionally exposes.
// None of them exist by default — a deployment has to declare them — so their
// absence is normal and never an error.
const (
	envPodName      = "POD_NAME"
	envHostname     = "HOSTNAME"
	envNamespaceVar = "POD_NAMESPACE"
	envClusterName  = "CLUSTER_NAME"
	kubernetesHost  = "KUBERNETES_SERVICE_HOST"
	namespaceFile   = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	providerK8s     = "kubernetes"
)

// detectInfrastructure recognises a Kubernetes pod when it sees one.
func detectInfrastructure() InfrastructureInfo {
	// KUBERNETES_SERVICE_HOST is injected into every pod by the kubelet, which
	// makes it the one reliable "am I in Kubernetes" signal. The rest is
	// best-effort.
	if os.Getenv(kubernetesHost) == "" {
		return InfrastructureInfo{}
	}

	info := InfrastructureInfo{
		Provider:  providerK8s,
		Namespace: firstNonEmpty(os.Getenv(envNamespaceVar), readNamespaceFile()),
		Cluster:   os.Getenv(envClusterName),
		Pod:       firstNonEmpty(os.Getenv(envPodName), os.Getenv(envHostname)),
	}
	return info
}

// readNamespaceFile reads the namespace every pod's service account token
// mount carries. It is the fallback when the downward API was not configured.
func readNamespaceFile() string {
	content, err := os.ReadFile(namespaceFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// newSessionID generates the identifier of one process execution.
//
// It is a UUIDv4 built here rather than pulled from a dependency: the SDK is
// imported into other people's binaries, and a profiler that drags in
// libraries is a profiler people refuse to adopt.
func newSessionID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// Cannot happen in practice; a time-free fallback keeps this function
		// total rather than panicking inside somebody's init.
		return "session-" + hex.EncodeToString([]byte(runtime.Version()))
	}

	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10

	encoded := hex.EncodeToString(buf)
	return strings.Join([]string{
		encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32],
	}, "-")
}
