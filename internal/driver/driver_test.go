package driver

import (
	"context"
	"log/slog"
	"os"
	"testing"

	pb "github.com/zanetworker/openshell-driver-openshift/gen/computev1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func newTestDriver(t *testing.T, objects ...runtime.Object) *Driver {
	t.Helper()

	scheme := runtime.NewScheme()

	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{
			sandboxGVR: "SandboxList",
		},
		objects...,
	)

	clientset := kubefake.NewSimpleClientset()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))

	return NewWithClients(dynClient, clientset, "test-ns", logger)
}

func TestGetCapabilities(t *testing.T) {
	d := newTestDriver(t)
	resp, err := d.GetCapabilities(context.Background(), &pb.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.DriverName != "openshift" {
		t.Errorf("expected driver name openshift, got %s", resp.DriverName)
	}
	if !resp.SupportsGpu {
		t.Error("expected GPU support")
	}
}

func TestCreateSandbox_Success(t *testing.T) {
	d := newTestDriver(t)
	ctx := context.Background()

	req := &pb.CreateSandboxRequest{
		Sandbox: &pb.DriverSandbox{
			Id:        "sb-001",
			Name:      "my-sandbox",
			Namespace: "test-ns",
			Spec: &pb.DriverSandboxSpec{
				Template: &pb.DriverSandboxTemplate{
					Image: "ghcr.io/nvidia/openshell-community/sandboxes/base:latest",
				},
			},
		},
	}

	_, err := d.CreateSandbox(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the CRD was created.
	resp, err := d.ListSandboxes(ctx, &pb.ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("list error: %v", err)
	}
	if len(resp.Sandboxes) != 1 {
		t.Fatalf("expected 1 sandbox, got %d", len(resp.Sandboxes))
	}
	if resp.Sandboxes[0].Name != "my-sandbox" {
		t.Errorf("expected name my-sandbox, got %s", resp.Sandboxes[0].Name)
	}
}

func TestCreateSandbox_MissingID(t *testing.T) {
	d := newTestDriver(t)
	_, err := d.CreateSandbox(context.Background(), &pb.CreateSandboxRequest{
		Sandbox: &pb.DriverSandbox{
			Name: "my-sandbox",
			Spec: &pb.DriverSandboxSpec{
				Template: &pb.DriverSandboxTemplate{
					Image: "test:latest",
				},
			},
		},
	})
	if err == nil {
		t.Fatal("expected error for missing sandbox ID")
	}
	if s, ok := status.FromError(err); !ok || s.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

func TestCreateSandbox_MissingImage(t *testing.T) {
	d := newTestDriver(t)
	_, err := d.CreateSandbox(context.Background(), &pb.CreateSandboxRequest{
		Sandbox: &pb.DriverSandbox{
			Id:   "sb-001",
			Name: "my-sandbox",
			Spec: &pb.DriverSandboxSpec{
				Template: &pb.DriverSandboxTemplate{},
			},
		},
	})
	if err == nil {
		t.Fatal("expected error for missing image")
	}
	if s, ok := status.FromError(err); !ok || s.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

func TestDeleteSandbox(t *testing.T) {
	d := newTestDriver(t)
	ctx := context.Background()

	// Create first.
	_, err := d.CreateSandbox(ctx, &pb.CreateSandboxRequest{
		Sandbox: &pb.DriverSandbox{
			Id:   "sb-del",
			Name: "to-delete",
			Spec: &pb.DriverSandboxSpec{
				Template: &pb.DriverSandboxTemplate{
					Image: "test:latest",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Delete.
	resp, err := d.DeleteSandbox(ctx, &pb.DeleteSandboxRequest{
		SandboxId:   "sb-del",
		SandboxName: "to-delete",
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !resp.Deleted {
		t.Error("expected deleted=true")
	}
}

func TestGetSandbox_NotFound(t *testing.T) {
	d := newTestDriver(t)
	_, err := d.GetSandbox(context.Background(), &pb.GetSandboxRequest{
		SandboxName: "nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent sandbox")
	}
	if s, ok := status.FromError(err); !ok || s.Code() != codes.NotFound {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestValidateSandboxCreate_GPUNoCapacity(t *testing.T) {
	// Create a fake clientset with a node that has no GPU.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
	}
	clientset := kubefake.NewSimpleClientset(node)

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{sandboxGVR: "SandboxList"},
	)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	d := NewWithClients(dynClient, clientset, "test-ns", logger)

	_, err := d.ValidateSandboxCreate(context.Background(), &pb.ValidateSandboxCreateRequest{
		Sandbox: &pb.DriverSandbox{
			Spec: &pb.DriverSandboxSpec{Gpu: true},
		},
	})
	if err == nil {
		t.Fatal("expected error when no GPU capacity")
	}
	if s, ok := status.FromError(err); !ok || s.Code() != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition, got %v", err)
	}
}

func TestResolveSandboxEndpoint_FallbackDNS(t *testing.T) {
	d := newTestDriver(t)

	resp, err := d.ResolveSandboxEndpoint(context.Background(),
		&pb.ResolveSandboxEndpointRequest{
			Sandbox: &pb.DriverSandbox{
				Name:      "my-sb",
				Namespace: "test-ns",
			},
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	host := resp.Endpoint.GetHost()
	expected := "my-sb.test-ns.svc.cluster.local"
	if host != expected {
		t.Errorf("expected DNS fallback %s, got %s", expected, host)
	}
	if resp.Endpoint.Port != sshPort {
		t.Errorf("expected port %d, got %d", sshPort, resp.Endpoint.Port)
	}
}

func TestListSandboxes_Empty(t *testing.T) {
	d := newTestDriver(t)
	resp, err := d.ListSandboxes(context.Background(), &pb.ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Sandboxes) != 0 {
		t.Errorf("expected 0 sandboxes, got %d", len(resp.Sandboxes))
	}
}

func TestListSandboxes_AfterCreate(t *testing.T) {
	d := newTestDriver(t)
	ctx := context.Background()

	// Prepopulate with an unstructured object.
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "agents.x-k8s.io/v1alpha1",
			"kind":       "Sandbox",
			"metadata": map[string]interface{}{
				"name":      "existing-sb",
				"namespace": "test-ns",
				"labels": map[string]interface{}{
					"openshell.ai/sandbox-id": "sb-existing",
				},
			},
		},
	}
	_, err := d.dynamic.Resource(sandboxGVR).
		Namespace("test-ns").
		Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("pre-create: %v", err)
	}

	resp, err := d.ListSandboxes(ctx, &pb.ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Sandboxes) != 1 {
		t.Errorf("expected 1 sandbox, got %d", len(resp.Sandboxes))
	}
}
