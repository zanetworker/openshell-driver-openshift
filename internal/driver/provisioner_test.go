package driver

import (
	"context"
	"log/slog"
	"os"
	"testing"

	pb "github.com/zanetworker/openshell-driver-openshift/gen/computev1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func newProvisionerForTest(t *testing.T) *K8sProvisioner {
	t.Helper()
	p, _ := newTestProvisioner(t)
	return p
}

func TestK8sProvisioner_CreateAndGet(t *testing.T) {
	p := newProvisionerForTest(t)
	ctx := context.Background()

	sb := &pb.DriverSandbox{
		Id:   "sb-100",
		Name: "prov-test",
		Spec: &pb.DriverSandboxSpec{
			Template: &pb.DriverSandboxTemplate{
				Image: "test:latest",
			},
		},
	}

	if err := p.Create(ctx, sb); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := p.Get(ctx, "prov-test")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Id != "sb-100" {
		t.Errorf("expected id sb-100, got %s", got.Id)
	}
	if got.Name != "prov-test" {
		t.Errorf("expected name prov-test, got %s", got.Name)
	}
}

func TestK8sProvisioner_CreateAndDelete(t *testing.T) {
	p := newProvisionerForTest(t)
	ctx := context.Background()

	sb := &pb.DriverSandbox{
		Id:   "sb-del",
		Name: "delete-me",
		Spec: &pb.DriverSandboxSpec{
			Template: &pb.DriverSandboxTemplate{
				Image: "test:latest",
			},
		},
	}
	if err := p.Create(ctx, sb); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := p.Delete(ctx, "delete-me"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Verify gone.
	_, err := p.Get(ctx, "delete-me")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestK8sProvisioner_List(t *testing.T) {
	p := newProvisionerForTest(t)
	ctx := context.Background()

	// Start empty.
	list, err := p.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0, got %d", len(list))
	}

	// Create two sandboxes.
	for _, name := range []string{"a", "b"} {
		if err := p.Create(ctx, &pb.DriverSandbox{
			Id:   "id-" + name,
			Name: name,
			Spec: &pb.DriverSandboxSpec{
				Template: &pb.DriverSandboxTemplate{Image: "img:latest"},
			},
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	list, err = p.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2, got %d", len(list))
	}
}

func TestK8sProvisioner_ValidateCreate_NoGPU(t *testing.T) {
	p := newProvisionerForTest(t)
	ctx := context.Background()

	sb := &pb.DriverSandbox{
		Spec: &pb.DriverSandboxSpec{Gpu: true},
	}
	err := p.ValidateCreate(ctx, sb)
	if err == nil {
		t.Fatal("expected error for GPU request with no GPU nodes")
	}
}

func TestK8sProvisioner_ValidateCreate_NoGPURequested(t *testing.T) {
	p := newProvisionerForTest(t)
	ctx := context.Background()

	sb := &pb.DriverSandbox{
		Spec: &pb.DriverSandboxSpec{Gpu: false},
	}
	err := p.ValidateCreate(ctx, sb)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestK8sProvisioner_ResolveEndpoint_DNS(t *testing.T) {
	p := newProvisionerForTest(t)
	ctx := context.Background()

	sb := &pb.DriverSandbox{
		Name:      "my-sb",
		Namespace: "test-ns",
	}

	ep, err := p.ResolveEndpoint(ctx, sb)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	expected := "my-sb.test-ns.svc.cluster.local"
	if ep.GetHost() != expected {
		t.Errorf("expected %s, got %s", expected, ep.GetHost())
	}
	if ep.Port != sshPort {
		t.Errorf("expected port %d, got %d", sshPort, ep.Port)
	}
}

func TestBuildSandboxSpec_SupervisorInitContainer(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Namespace = "test-ns"

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{sandboxGVR: "SandboxList"},
	)
	clientset := kubefake.NewSimpleClientset()
	p := NewK8sProvisioner(dynClient, clientset, cfg, logger)

	sb := &pb.DriverSandbox{
		Id: "sb-init",
		Spec: &pb.DriverSandboxSpec{
			Template: &pb.DriverSandboxTemplate{
				Image: "agent:latest",
			},
		},
	}

	spec := p.buildSandboxSpec(sb)

	// Verify podTemplate structure.
	podTemplate, ok := spec["podTemplate"].(map[string]interface{})
	if !ok {
		t.Fatal("missing podTemplate")
	}
	podSpec, ok := podTemplate["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("missing podTemplate.spec")
	}

	// Verify init containers.
	initContainers, ok := podSpec["initContainers"].([]interface{})
	if !ok || len(initContainers) == 0 {
		t.Fatal("missing initContainers")
	}
	initC := initContainers[0].(map[string]interface{})
	if initC["name"] != "supervisor-init" {
		t.Errorf("expected init container name supervisor-init, got %v", initC["name"])
	}
	if initC["image"] != cfg.SupervisorImage {
		t.Errorf("expected image %s, got %v", cfg.SupervisorImage, initC["image"])
	}

	// Verify command copies supervisor binary.
	cmd := initC["command"].([]interface{})
	if len(cmd) != 3 || cmd[0] != "cp" {
		t.Errorf("expected cp command, got %v", cmd)
	}
	if cmd[1] != cfg.SupervisorBinaryPath {
		t.Errorf("expected source %s, got %v", cfg.SupervisorBinaryPath, cmd[1])
	}

	// Verify agent container runs supervisor.
	containers, ok := podSpec["containers"].([]interface{})
	if !ok || len(containers) == 0 {
		t.Fatal("missing containers")
	}
	agentC := containers[0].(map[string]interface{})
	if agentC["name"] != "agent" {
		t.Errorf("expected container name agent, got %v", agentC["name"])
	}
	agentCmd := agentC["command"].([]interface{})
	expectedCmd := cfg.SupervisorMountPath + "/openshell-sandbox"
	if len(agentCmd) != 1 || agentCmd[0] != expectedCmd {
		t.Errorf("expected command [%s], got %v", expectedCmd, agentCmd)
	}

	// Verify security context.
	secCtx := agentC["securityContext"].(map[string]interface{})
	if secCtx["runAsUser"] != int64(0) {
		t.Errorf("expected runAsUser 0, got %v", secCtx["runAsUser"])
	}
	caps := secCtx["capabilities"].(map[string]interface{})
	addCaps := caps["add"].([]interface{})
	expectedCaps := []string{"SYS_ADMIN", "NET_ADMIN", "SYS_PTRACE", "SYSLOG"}
	if len(addCaps) != len(expectedCaps) {
		t.Fatalf("expected %d capabilities, got %d", len(expectedCaps), len(addCaps))
	}
	for i, c := range expectedCaps {
		if addCaps[i] != c {
			t.Errorf("expected capability %s at index %d, got %v", c, i, addCaps[i])
		}
	}

	// Verify volume mounts on agent container.
	agentMounts := agentC["volumeMounts"].([]interface{})
	if len(agentMounts) != 1 {
		t.Fatalf("expected 1 volume mount on agent, got %d", len(agentMounts))
	}
	mount := agentMounts[0].(map[string]interface{})
	if mount["name"] != "supervisor-bin" {
		t.Errorf("expected mount name supervisor-bin, got %v", mount["name"])
	}
	if mount["readOnly"] != true {
		t.Error("expected readOnly=true on agent volume mount")
	}

	// Verify volumes.
	volumes, ok := podSpec["volumes"].([]interface{})
	if !ok || len(volumes) == 0 {
		t.Fatal("missing volumes")
	}
	vol := volumes[0].(map[string]interface{})
	if vol["name"] != "supervisor-bin" {
		t.Errorf("expected volume name supervisor-bin, got %v", vol["name"])
	}
	if _, ok := vol["emptyDir"]; !ok {
		t.Error("expected emptyDir volume")
	}
}

func TestBuildSandboxSpec_Labels(t *testing.T) {
	p := newProvisionerForTest(t)

	sb := &pb.DriverSandbox{
		Id: "sb-labels",
		Spec: &pb.DriverSandboxSpec{
			Template: &pb.DriverSandboxTemplate{
				Image: "img:latest",
				Labels: map[string]string{
					"custom": "label",
				},
			},
		},
	}

	spec := p.buildSandboxSpec(sb)
	podTemplate := spec["podTemplate"].(map[string]interface{})
	meta := podTemplate["metadata"].(map[string]interface{})
	labels := meta["labels"].(map[string]interface{})

	if labels["custom"] != "label" {
		t.Errorf("expected custom=label, got %v", labels["custom"])
	}
	if labels[labelSandboxID] != "sb-labels" {
		t.Errorf("expected sandbox ID label, got %v", labels[labelSandboxID])
	}
	if labels[labelManagedBy] != "openshell" {
		t.Errorf("expected managed-by label, got %v", labels[labelManagedBy])
	}
}

func TestNewWithDeps(t *testing.T) {
	p := newProvisionerForTest(t)
	logger := testLogger()

	d := NewWithDeps(p, &NoopEnricher{}, &NoopMetrics{}, logger)

	resp, err := d.GetCapabilities(context.Background(), &pb.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.DriverName != "openshift" {
		t.Errorf("expected openshift, got %s", resp.DriverName)
	}
}

func TestK8sProvisioner_Watch_ChannelCloses(t *testing.T) {
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{sandboxGVR: "SandboxList"},
	)
	clientset := kubefake.NewSimpleClientset()
	logger := testLogger()
	cfg := testConfig()

	p := NewK8sProvisioner(dynClient, clientset, cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())

	ch, err := p.Watch(ctx)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}

	// Cancel context to stop the watcher; the channel should close.
	cancel()

	// Drain and verify the channel closes without hanging.
	for range ch {
	}
}
