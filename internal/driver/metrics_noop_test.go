package driver

import (
	"testing"
	"time"
)

func TestNoopMetrics_DoesNotPanic(t *testing.T) {
	m := &NoopMetrics{}
	m.SandboxCreated("test", true, 5*time.Second)
	m.SandboxDeleted("test")
	m.SandboxFailed("test", "reason")
	m.WatchEventReceived("ADDED")
}
