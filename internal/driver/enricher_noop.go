package driver

import "context"

// NoopEnricher is a PlatformEnricher that does nothing. Used in Phase 1
// before OpenShift-specific enrichment is implemented.
type NoopEnricher struct{}

func (n *NoopEnricher) DetectSCC(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (n *NoopEnricher) DetectSELinuxType(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (n *NoopEnricher) EnrichPodSpec(podSpec map[string]interface{}, _ string) (map[string]interface{}, error) {
	return podSpec, nil
}
