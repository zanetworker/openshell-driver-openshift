package driver

import (
	"context"
	"testing"
	"time"

	pb "github.com/zanetworker/openshell-driver-openshift/gen/computev1"
)

// Compile-time interface satisfaction checks. These verify that a concrete
// type can implement each interface, catching signature drift early.

// stubProvisioner is a minimal SandboxProvisioner implementation for testing.
type stubProvisioner struct{}

func (s *stubProvisioner) Create(_ context.Context, _ *pb.DriverSandbox) error  { return nil }
func (s *stubProvisioner) Delete(_ context.Context, _ string) error             { return nil }
func (s *stubProvisioner) Get(_ context.Context, _ string) (*pb.DriverSandbox, error) {
	return nil, nil
}
func (s *stubProvisioner) List(_ context.Context) ([]*pb.DriverSandbox, error) { return nil, nil }
func (s *stubProvisioner) Watch(_ context.Context) (<-chan WatchEvent, error)   { return nil, nil }
func (s *stubProvisioner) ResolveEndpoint(_ context.Context, _ *pb.DriverSandbox) (*pb.SandboxEndpoint, error) {
	return nil, nil
}
func (s *stubProvisioner) ValidateCreate(_ context.Context, _ *pb.DriverSandbox) error { return nil }
func (s *stubProvisioner) HasGPUCapacity(_ context.Context) (bool, error)              { return false, nil }

var _ SandboxProvisioner = (*stubProvisioner)(nil)

// stubEnricher is a minimal PlatformEnricher implementation for testing.
type stubEnricher struct{}

func (s *stubEnricher) DetectSCC(_ context.Context, _ string) (string, error)       { return "", nil }
func (s *stubEnricher) DetectSELinuxType(_ context.Context, _ string) (string, error) { return "", nil }
func (s *stubEnricher) EnrichPodSpec(podSpec map[string]interface{}, _ string) (map[string]interface{}, error) {
	return podSpec, nil
}

var _ PlatformEnricher = (*stubEnricher)(nil)

// stubMetrics is a minimal DriverMetrics implementation for testing.
type stubMetrics struct{}

func (s *stubMetrics) SandboxCreated(_ string, _ bool, _ time.Duration) {}
func (s *stubMetrics) SandboxDeleted(_ string)                         {}
func (s *stubMetrics) SandboxFailed(_ string, _ string)                {}
func (s *stubMetrics) WatchEventReceived(_ string)                     {}

var _ DriverMetrics = (*stubMetrics)(nil)

func TestWatchEventTypeConstants(t *testing.T) {
	if WatchEventUpdated != 0 {
		t.Errorf("WatchEventUpdated = %d, want 0", WatchEventUpdated)
	}
	if WatchEventDeleted != 1 {
		t.Errorf("WatchEventDeleted = %d, want 1", WatchEventDeleted)
	}
}

func TestWatchEventStruct(t *testing.T) {
	sb := &pb.DriverSandbox{}
	evt := WatchEvent{
		Type:      WatchEventDeleted,
		Sandbox:   sb,
		SandboxID: "test-sandbox-123",
	}

	if evt.Type != WatchEventDeleted {
		t.Errorf("WatchEvent.Type = %d, want WatchEventDeleted", evt.Type)
	}
	if evt.Sandbox != sb {
		t.Error("WatchEvent.Sandbox not set correctly")
	}
	if evt.SandboxID != "test-sandbox-123" {
		t.Errorf("WatchEvent.SandboxID = %q, want %q", evt.SandboxID, "test-sandbox-123")
	}
}
