package driver

import (
	"context"
	"time"

	pb "github.com/zanetworker/openshell-driver-openshift/gen/computev1"
)

// SandboxProvisioner handles sandbox lifecycle on K8s.
type SandboxProvisioner interface {
	Create(ctx context.Context, sb *pb.DriverSandbox) error
	Delete(ctx context.Context, name string) error
	Get(ctx context.Context, name string) (*pb.DriverSandbox, error)
	List(ctx context.Context) ([]*pb.DriverSandbox, error)
	Watch(ctx context.Context) (<-chan WatchEvent, error)
	ValidateCreate(ctx context.Context, sb *pb.DriverSandbox) error
	HasGPUCapacity(ctx context.Context) (bool, error)
}

// WatchEvent represents a sandbox state change from the K8s watcher.
type WatchEvent struct {
	Type    WatchEventType
	Sandbox *pb.DriverSandbox
	// SandboxID is set only for Deleted events.
	SandboxID string
}

// WatchEventType distinguishes sandbox watch events.
type WatchEventType int

const (
	WatchEventUpdated WatchEventType = iota
	WatchEventDeleted
)

// PlatformEnricher adds OpenShift-specific behavior to sandbox pod specs.
// Phase 1 uses the noop implementation; Phase 2 adds the real one.
type PlatformEnricher interface {
	DetectSCC(ctx context.Context, namespace string) (string, error)
	DetectSELinuxType(ctx context.Context, namespace string) (string, error)
	EnrichPodSpec(podSpec map[string]interface{}, namespace string) (map[string]interface{}, error)
}

// DriverMetrics tracks driver-level observability counters.
// Phase 1 uses the noop implementation; Phase 2 adds Prometheus.
type DriverMetrics interface {
	SandboxCreated(name string, gpu bool, duration time.Duration)
	SandboxDeleted(name string)
	SandboxFailed(name string, reason string)
	WatchEventReceived(eventType string)
}
