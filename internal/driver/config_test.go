package driver

import "testing"

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	tests := []struct {
		field string
		got   string
		want  string
	}{
		{"Namespace", cfg.Namespace, "openshell-system"},
		{"SupervisorImage", cfg.SupervisorImage, "ghcr.io/nvidia/openshell-community/supervisor:latest"},
		{"SupervisorBinaryPath", cfg.SupervisorBinaryPath, "/usr/local/bin/openshell-sandbox"},
		{"SupervisorMountPath", cfg.SupervisorMountPath, "/opt/openshell/bin"},
	}

	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("DefaultConfig().%s = %q, want %q", tt.field, tt.got, tt.want)
		}
	}
}

func TestConfigZeroValue(t *testing.T) {
	var cfg Config

	if cfg.Namespace != "" {
		t.Errorf("zero-value Config.Namespace = %q, want empty", cfg.Namespace)
	}
	if cfg.SupervisorImage != "" {
		t.Errorf("zero-value Config.SupervisorImage = %q, want empty", cfg.SupervisorImage)
	}
	if cfg.SupervisorBinaryPath != "" {
		t.Errorf("zero-value Config.SupervisorBinaryPath = %q, want empty", cfg.SupervisorBinaryPath)
	}
	if cfg.SupervisorMountPath != "" {
		t.Errorf("zero-value Config.SupervisorMountPath = %q, want empty", cfg.SupervisorMountPath)
	}
}
