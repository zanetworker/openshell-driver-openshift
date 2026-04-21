package driver

import (
	"context"
	"testing"
)

func TestNoopEnricher_DetectSCC_ReturnsEmpty(t *testing.T) {
	e := &NoopEnricher{}
	scc, err := e.DetectSCC(context.Background(), "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scc != "" {
		t.Errorf("expected empty SCC, got %s", scc)
	}
}

func TestNoopEnricher_DetectSELinuxType_ReturnsEmpty(t *testing.T) {
	e := &NoopEnricher{}
	sel, err := e.DetectSELinuxType(context.Background(), "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel != "" {
		t.Errorf("expected empty SELinux type, got %s", sel)
	}
}

func TestNoopEnricher_EnrichPodSpec_PassesThrough(t *testing.T) {
	e := &NoopEnricher{}
	input := map[string]interface{}{"containers": []interface{}{}}
	output, err := e.EnrichPodSpec(input, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := output["containers"]; !ok {
		t.Error("expected containers key to survive passthrough")
	}
}
