package driver

import "time"

// NoopMetrics is a DriverMetrics that discards all observations.
// Used in Phase 1 before Prometheus metrics are implemented.
type NoopMetrics struct{}

func (n *NoopMetrics) SandboxCreated(_ string, _ bool, _ time.Duration) {}
func (n *NoopMetrics) SandboxDeleted(_ string)                          {}
func (n *NoopMetrics) SandboxFailed(_ string, _ string)                 {}
func (n *NoopMetrics) WatchEventReceived(_ string)                      {}
