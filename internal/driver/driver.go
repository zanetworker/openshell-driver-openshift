// Package driver implements the OpenShell ComputeDriver gRPC service for
// OpenShift/Kubernetes clusters. It creates agents.x-k8s.io/v1alpha1 Sandbox
// CRDs and watches them for status changes, reporting observations back to the
// OpenShell gateway over the gRPC stream.
package driver

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	pb "github.com/zanetworker/openshell-driver-openshift/gen/computev1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Driver implements the ComputeDriverServer gRPC interface. It delegates
// sandbox lifecycle to a SandboxProvisioner and uses PlatformEnricher and
// DriverMetrics for cross-cutting concerns.
type Driver struct {
	pb.UnimplementedComputeDriverServer

	provisioner SandboxProvisioner
	enricher    PlatformEnricher
	metrics     DriverMetrics
	logger      *slog.Logger
}

// New creates a Driver using the best available K8s config: in-cluster if
// running inside a pod, or kubeconfig from KUBECONFIG / ~/.kube/config
// when running locally.
func New(cfg Config, logger *slog.Logger) (*Driver, error) {
	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig for local development.
		kubeconfigPath := os.Getenv("KUBECONFIG")
		if kubeconfigPath == "" {
			home, _ := os.UserHomeDir()
			kubeconfigPath = home + "/.kube/config"
		}
		kubeConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, fmt.Errorf("build kubeconfig from %s: %w", kubeconfigPath, err)
		}
		logger.Info("using kubeconfig", "path", kubeconfigPath)
	}

	dynClient, err := dynamic.NewForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}

	return NewWithClients(dynClient, clientset, cfg, logger), nil
}

// NewWithClients creates a Driver with pre-built K8s clients. Use this for
// testing with fake clients or when the caller manages client lifecycle.
func NewWithClients(
	dynClient dynamic.Interface,
	clientset kubernetes.Interface,
	cfg Config,
	logger *slog.Logger,
) *Driver {
	provisioner := NewK8sProvisioner(dynClient, clientset, cfg, logger)
	return NewWithDeps(provisioner, &NoopEnricher{}, &NoopMetrics{}, logger)
}

// NewWithDeps creates a Driver with fully injected dependencies. Use this
// for unit tests that provide mock implementations of all interfaces.
func NewWithDeps(
	provisioner SandboxProvisioner,
	enricher PlatformEnricher,
	metrics DriverMetrics,
	logger *slog.Logger,
) *Driver {
	return &Driver{
		provisioner: provisioner,
		enricher:    enricher,
		metrics:     metrics,
		logger:      logger,
	}
}

func (d *Driver) GetCapabilities(
	_ context.Context,
	_ *pb.GetCapabilitiesRequest,
) (*pb.GetCapabilitiesResponse, error) {
	return &pb.GetCapabilitiesResponse{
		DriverName:    "openshift",
		DriverVersion: "0.1.0",
		DefaultImage:  "ghcr.io/nvidia/openshell-community/sandboxes/base:latest",
		SupportsGpu:   true,
	}, nil
}

func (d *Driver) ValidateSandboxCreate(
	ctx context.Context,
	req *pb.ValidateSandboxCreateRequest,
) (*pb.ValidateSandboxCreateResponse, error) {
	if err := d.provisioner.ValidateCreate(ctx, req.GetSandbox()); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	return &pb.ValidateSandboxCreateResponse{}, nil
}

func (d *Driver) CreateSandbox(
	ctx context.Context,
	req *pb.CreateSandboxRequest,
) (*pb.CreateSandboxResponse, error) {
	sb := req.GetSandbox()
	if sb.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox id is required")
	}
	if sb.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox name is required")
	}
	spec := sb.GetSpec()
	if spec == nil {
		return nil, status.Error(codes.InvalidArgument, "sandbox spec is required")
	}
	tmpl := spec.GetTemplate()
	if tmpl == nil || tmpl.GetImage() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox template with image is required")
	}

	start := time.Now()
	if err := d.provisioner.Create(ctx, sb); err != nil {
		d.metrics.SandboxFailed(sb.GetName(), err.Error())
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	d.metrics.SandboxCreated(sb.GetName(), spec.GetGpu(), time.Since(start))

	return &pb.CreateSandboxResponse{}, nil
}

func (d *Driver) GetSandbox(
	ctx context.Context,
	req *pb.GetSandboxRequest,
) (*pb.GetSandboxResponse, error) {
	sb, err := d.provisioner.Get(ctx, req.GetSandboxName())
	if err != nil {
		return nil, status.Errorf(codes.NotFound,
			"sandbox %s not found: %v", req.GetSandboxName(), err)
	}
	return &pb.GetSandboxResponse{Sandbox: sb}, nil
}

func (d *Driver) ListSandboxes(
	ctx context.Context,
	_ *pb.ListSandboxesRequest,
) (*pb.ListSandboxesResponse, error) {
	sandboxes, err := d.provisioner.List(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list sandboxes: %v", err)
	}
	return &pb.ListSandboxesResponse{Sandboxes: sandboxes}, nil
}

func (d *Driver) StopSandbox(
	ctx context.Context,
	req *pb.StopSandboxRequest,
) (*pb.StopSandboxResponse, error) {
	if err := d.provisioner.Delete(ctx, req.GetSandboxName()); err != nil {
		return nil, status.Errorf(codes.Internal,
			"stop sandbox %s: %v", req.GetSandboxName(), err)
	}
	return &pb.StopSandboxResponse{}, nil
}

func (d *Driver) DeleteSandbox(
	ctx context.Context,
	req *pb.DeleteSandboxRequest,
) (*pb.DeleteSandboxResponse, error) {
	if err := d.provisioner.Delete(ctx, req.GetSandboxName()); err != nil {
		d.metrics.SandboxFailed(req.GetSandboxName(), err.Error())
		return nil, status.Errorf(codes.Internal,
			"delete sandbox %s: %v", req.GetSandboxName(), err)
	}
	d.metrics.SandboxDeleted(req.GetSandboxName())
	d.logger.Info("sandbox deleted", "name", req.GetSandboxName(), "id", req.GetSandboxId())
	return &pb.DeleteSandboxResponse{Deleted: true}, nil
}

func (d *Driver) ResolveSandboxEndpoint(
	ctx context.Context,
	req *pb.ResolveSandboxEndpointRequest,
) (*pb.ResolveSandboxEndpointResponse, error) {
	endpoint, err := d.provisioner.ResolveEndpoint(ctx, req.GetSandbox())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve endpoint: %v", err)
	}
	return &pb.ResolveSandboxEndpointResponse{Endpoint: endpoint}, nil
}

func (d *Driver) WatchSandboxes(
	_ *pb.WatchSandboxesRequest,
	stream grpc.ServerStreamingServer[pb.WatchSandboxesEvent],
) error {
	ch, err := d.provisioner.Watch(stream.Context())
	if err != nil {
		return status.Errorf(codes.Internal, "%v", err)
	}

	for event := range ch {
		var evt *pb.WatchSandboxesEvent

		switch event.Type {
		case WatchEventUpdated:
			d.metrics.WatchEventReceived("updated")
			evt = &pb.WatchSandboxesEvent{
				Payload: &pb.WatchSandboxesEvent_Sandbox{
					Sandbox: &pb.WatchSandboxesSandboxEvent{
						Sandbox: event.Sandbox,
					},
				},
			}
		case WatchEventDeleted:
			d.metrics.WatchEventReceived("deleted")
			evt = &pb.WatchSandboxesEvent{
				Payload: &pb.WatchSandboxesEvent_Deleted{
					Deleted: &pb.WatchSandboxesDeletedEvent{
						SandboxId: event.SandboxID,
					},
				},
			}
		default:
			continue
		}

		if err := stream.Send(evt); err != nil {
			return err
		}
	}

	return nil
}
