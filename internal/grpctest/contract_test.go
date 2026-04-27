package grpctest

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/zanetworker/openshell-driver-openshift/gen/computev1"
	"github.com/zanetworker/openshell-driver-openshift/internal/driver"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

// sandboxGVR mirrors the GVR defined in the driver package. We define it
// locally because the driver package does not export it.
var sandboxGVR = schema.GroupVersionResource{
	Group:    "agents.x-k8s.io",
	Version:  "v1alpha1",
	Resource: "sandboxes",
}

// startTestServer creates a gRPC server backed by fake K8s clients, listens on
// a temporary Unix domain socket, and returns a connected client plus a cleanup
// function. The caller should defer the cleanup.
func startTestServer(t *testing.T) (pb.ComputeDriverClient, func()) {
	t.Helper()

	// Use os.MkdirTemp in /tmp directly to keep the Unix socket path under
	// the 108-character limit. t.TempDir() nests under the test name which
	// can exceed the limit for long test names.
	tmpDir, err := os.MkdirTemp("", "grpctest")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	socketPath := filepath.Join(tmpDir, "d.sock")

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on UDS: %v", err)
	}

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{
			sandboxGVR: "SandboxList",
		},
	)
	clientset := kubefake.NewSimpleClientset()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelError,
	}))

	cfg := driver.DefaultConfig()
	cfg.Namespace = "test-ns"

	drv := driver.NewWithClients(dynClient, clientset, cfg, logger)

	srv := grpc.NewServer()
	pb.RegisterComputeDriverServer(srv, drv)

	go func() {
		if err := srv.Serve(lis); err != nil {
			// Serve returns an error after GracefulStop; ignore.
		}
	}()

	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		srv.Stop()
		t.Fatalf("dial UDS: %v", err)
	}

	client := pb.NewComputeDriverClient(conn)

	cleanup := func() {
		conn.Close()
		srv.GracefulStop()
	}

	return client, cleanup
}

// validSandbox returns a minimal DriverSandbox that passes CreateSandbox
// validation.
func validSandbox(id, name string) *pb.DriverSandbox {
	return &pb.DriverSandbox{
		Id:   id,
		Name: name,
		Spec: &pb.DriverSandboxSpec{
			Template: &pb.DriverSandboxTemplate{
				Image: "ghcr.io/test/sandbox:latest",
			},
		},
	}
}

func TestGRPC_GetCapabilities(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	resp, err := client.GetCapabilities(context.Background(), &pb.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if resp.DriverName != "openshift" {
		t.Errorf("expected driver name 'openshift', got %q", resp.DriverName)
	}
	if resp.DriverVersion == "" {
		t.Error("expected non-empty driver version")
	}
	if !resp.SupportsGpu {
		t.Error("expected SupportsGpu=true")
	}
}

func TestGRPC_CreateAndGetSandbox(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()

	// Create a sandbox.
	sb := validSandbox("sb-001", "grpc-test-sandbox")
	_, err := client.CreateSandbox(ctx, &pb.CreateSandboxRequest{Sandbox: sb})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	// Get it back.
	getResp, err := client.GetSandbox(ctx, &pb.GetSandboxRequest{
		SandboxName: "grpc-test-sandbox",
	})
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if getResp.Sandbox.GetName() != "grpc-test-sandbox" {
		t.Errorf("expected name 'grpc-test-sandbox', got %q", getResp.Sandbox.GetName())
	}
	if getResp.Sandbox.GetId() != "sb-001" {
		t.Errorf("expected id 'sb-001', got %q", getResp.Sandbox.GetId())
	}
}

func TestGRPC_CreateSandbox_InvalidArgument(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	// Missing ID should return InvalidArgument.
	_, err := client.CreateSandbox(context.Background(), &pb.CreateSandboxRequest{
		Sandbox: &pb.DriverSandbox{
			Name: "no-id",
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
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", st.Code())
	}
}

func TestGRPC_ListSandboxes_Empty(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	resp, err := client.ListSandboxes(context.Background(), &pb.ListSandboxesRequest{})
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	if len(resp.Sandboxes) != 0 {
		t.Errorf("expected 0 sandboxes, got %d", len(resp.Sandboxes))
	}
}

func TestGRPC_DeleteSandbox(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()

	// Create first.
	sb := validSandbox("sb-del", "to-delete")
	_, err := client.CreateSandbox(ctx, &pb.CreateSandboxRequest{Sandbox: sb})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	// Delete.
	delResp, err := client.DeleteSandbox(ctx, &pb.DeleteSandboxRequest{
		SandboxId:   "sb-del",
		SandboxName: "to-delete",
	})
	if err != nil {
		t.Fatalf("DeleteSandbox: %v", err)
	}
	if !delResp.Deleted {
		t.Error("expected Deleted=true")
	}

	// Verify it is gone.
	_, err = client.GetSandbox(ctx, &pb.GetSandboxRequest{
		SandboxName: "to-delete",
	})
	if err == nil {
		t.Fatal("expected error after deletion")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("expected NotFound after delete, got %s", st.Code())
	}
}

func TestGRPC_GetSandbox_NotFound(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	_, err := client.GetSandbox(context.Background(), &pb.GetSandboxRequest{
		SandboxName: "nonexistent",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent sandbox")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("expected NotFound, got %s", st.Code())
	}
}

func TestGRPC_StopSandbox_Unimplemented(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	_, err := client.StopSandbox(context.Background(), &pb.StopSandboxRequest{
		SandboxId:   "sb-stop",
		SandboxName: "stop-me",
	})
	if err == nil {
		t.Fatal("expected error from StopSandbox")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.Unimplemented {
		t.Errorf("expected Unimplemented, got %s", st.Code())
	}
}

func TestGRPC_WatchSandboxes_StreamEstablished(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, err := client.WatchSandboxes(ctx, &pb.WatchSandboxesRequest{})
	if err != nil {
		t.Fatalf("WatchSandboxes: %v", err)
	}

	// The stream should be established. Cancel the context to close it
	// cleanly, then verify Recv returns an error (expected after cancel).
	cancel()

	_, recvErr := stream.Recv()
	if recvErr == nil {
		t.Fatal("expected error after context cancellation, got nil")
	}
	// Any error is acceptable here; the point is the stream connected.
}
