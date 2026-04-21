// Package driver implements the OpenShell ComputeDriver gRPC service for
// OpenShift/Kubernetes clusters. It creates agents.x-k8s.io/v1alpha1 Sandbox
// CRDs and watches them for status changes, reporting observations back to the
// OpenShell gateway over the gRPC stream.
package driver

import (
	"context"
	"fmt"
	"log/slog"

	pb "github.com/zanetworker/openshell-driver-openshift/gen/computev1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var sandboxGVR = schema.GroupVersionResource{
	Group:    "agents.x-k8s.io",
	Version:  "v1alpha1",
	Resource: "sandboxes",
}

const (
	labelSandboxID = "openshell.ai/sandbox-id"
	labelManagedBy = "openshell.ai/managed-by"
	labelKagenti   = "kagenti.io/type"
	sshPort        = 2222
)

// Driver implements the ComputeDriverServer gRPC interface. It provisions
// sandboxes as agents.x-k8s.io/v1alpha1 Sandbox CRDs on an OpenShift cluster.
type Driver struct {
	pb.UnimplementedComputeDriverServer

	dynamic   dynamic.Interface
	clientset kubernetes.Interface
	namespace string
	logger    *slog.Logger
}

// New creates a Driver that targets the given namespace. It uses in-cluster
// config to authenticate to the Kubernetes API.
func New(namespace string, logger *slog.Logger) (*Driver, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("build in-cluster config: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}

	return NewWithClients(dynClient, clientset, namespace, logger), nil
}

// NewWithClients creates a Driver with pre-built K8s clients. Use this for
// testing with fake clients or when the caller manages client lifecycle.
func NewWithClients(
	dynClient dynamic.Interface,
	clientset kubernetes.Interface,
	namespace string,
	logger *slog.Logger,
) *Driver {
	return &Driver{
		dynamic:   dynClient,
		clientset: clientset,
		namespace: namespace,
		logger:    logger,
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
	sb := req.GetSandbox()
	if sb.GetSpec().GetGpu() {
		ok, err := d.hasGPUCapacity(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "check GPU capacity: %v", err)
		}
		if !ok {
			return nil, status.Error(codes.FailedPrecondition,
				"no nodes with nvidia.com/gpu allocatable in the cluster")
		}
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

	labels := mergeMaps(tmpl.GetLabels(), map[string]string{
		labelSandboxID: sb.GetId(),
		labelManagedBy: "openshell",
		labelKagenti:   "agent",
	})

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "agents.x-k8s.io/v1alpha1",
			"kind":       "Sandbox",
			"metadata": map[string]interface{}{
				"name":      sb.GetName(),
				"namespace": d.namespace,
				"labels":    labels,
			},
			"spec": d.buildPodSpec(sb),
		},
	}

	_, err := d.dynamic.Resource(sandboxGVR).
		Namespace(d.namespace).
		Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create Sandbox CR %s: %v",
			sb.GetName(), err)
	}

	d.logger.Info("sandbox created",
		"name", sb.GetName(),
		"id", sb.GetId(),
		"gpu", spec.GetGpu())
	return &pb.CreateSandboxResponse{}, nil
}

func (d *Driver) GetSandbox(
	ctx context.Context,
	req *pb.GetSandboxRequest,
) (*pb.GetSandboxResponse, error) {
	obj, err := d.dynamic.Resource(sandboxGVR).
		Namespace(d.namespace).
		Get(ctx, req.GetSandboxName(), metav1.GetOptions{})
	if err != nil {
		return nil, status.Errorf(codes.NotFound,
			"sandbox %s not found: %v", req.GetSandboxName(), err)
	}
	return &pb.GetSandboxResponse{Sandbox: objToDriverSandbox(obj)}, nil
}

func (d *Driver) ListSandboxes(
	ctx context.Context,
	_ *pb.ListSandboxesRequest,
) (*pb.ListSandboxesResponse, error) {
	list, err := d.dynamic.Resource(sandboxGVR).
		Namespace(d.namespace).
		List(ctx, metav1.ListOptions{
			LabelSelector: labelSandboxID,
		})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list sandboxes: %v", err)
	}

	sandboxes := make([]*pb.DriverSandbox, 0, len(list.Items))
	for i := range list.Items {
		sandboxes = append(sandboxes, objToDriverSandbox(&list.Items[i]))
	}
	return &pb.ListSandboxesResponse{Sandboxes: sandboxes}, nil
}

func (d *Driver) StopSandbox(
	ctx context.Context,
	req *pb.StopSandboxRequest,
) (*pb.StopSandboxResponse, error) {
	// Delete the sandbox CR; the agent-sandbox controller cleans up the pod.
	err := d.dynamic.Resource(sandboxGVR).
		Namespace(d.namespace).
		Delete(ctx, req.GetSandboxName(), metav1.DeleteOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"stop sandbox %s: %v", req.GetSandboxName(), err)
	}
	return &pb.StopSandboxResponse{}, nil
}

func (d *Driver) DeleteSandbox(
	ctx context.Context,
	req *pb.DeleteSandboxRequest,
) (*pb.DeleteSandboxResponse, error) {
	err := d.dynamic.Resource(sandboxGVR).
		Namespace(d.namespace).
		Delete(ctx, req.GetSandboxName(), metav1.DeleteOptions{})
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"delete sandbox %s: %v", req.GetSandboxName(), err)
	}
	d.logger.Info("sandbox deleted", "name", req.GetSandboxName(), "id", req.GetSandboxId())
	return &pb.DeleteSandboxResponse{Deleted: true}, nil
}

func (d *Driver) ResolveSandboxEndpoint(
	ctx context.Context,
	req *pb.ResolveSandboxEndpointRequest,
) (*pb.ResolveSandboxEndpointResponse, error) {
	sb := req.GetSandbox()

	// Try pod IP via the instance_id (agent pod name).
	if sts := sb.GetStatus(); sts != nil && sts.GetInstanceId() != "" {
		pod, err := d.clientset.CoreV1().Pods(d.namespace).
			Get(ctx, sts.GetInstanceId(), metav1.GetOptions{})
		if err != nil {
			d.logger.Warn("pod lookup failed, falling back to DNS",
				"pod", sts.GetInstanceId(), "error", err)
		} else if pod.Status.PodIP != "" {
			return &pb.ResolveSandboxEndpointResponse{
				Endpoint: &pb.SandboxEndpoint{
					Target: &pb.SandboxEndpoint_Ip{Ip: pod.Status.PodIP},
					Port:   sshPort,
				},
			}, nil
		}
	}

	// Fallback: cluster DNS.
	return &pb.ResolveSandboxEndpointResponse{
		Endpoint: &pb.SandboxEndpoint{
			Target: &pb.SandboxEndpoint_Host{
				Host: fmt.Sprintf("%s.%s.svc.cluster.local",
					sb.GetName(), d.namespace),
			},
			Port: sshPort,
		},
	}, nil
}

func (d *Driver) WatchSandboxes(
	_ *pb.WatchSandboxesRequest,
	stream grpc.ServerStreamingServer[pb.WatchSandboxesEvent],
) error {
	watcher, err := d.dynamic.Resource(sandboxGVR).
		Namespace(d.namespace).
		Watch(stream.Context(), metav1.ListOptions{
			LabelSelector: labelSandboxID,
		})
	if err != nil {
		return status.Errorf(codes.Internal, "start watcher: %v", err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		obj, ok := event.Object.(*unstructured.Unstructured)
		if !ok {
			continue
		}

		var evt *pb.WatchSandboxesEvent
		switch event.Type {
		case watch.Added, watch.Modified:
			evt = &pb.WatchSandboxesEvent{
				Payload: &pb.WatchSandboxesEvent_Sandbox{
					Sandbox: &pb.WatchSandboxesSandboxEvent{
						Sandbox: objToDriverSandbox(obj),
					},
				},
			}
		case watch.Deleted:
			sandboxID := obj.GetLabels()[labelSandboxID]
			evt = &pb.WatchSandboxesEvent{
				Payload: &pb.WatchSandboxesEvent_Deleted{
					Deleted: &pb.WatchSandboxesDeletedEvent{
						SandboxId: sandboxID,
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

// buildPodSpec constructs the Sandbox CR spec from driver-native messages.
func (d *Driver) buildPodSpec(sb *pb.DriverSandbox) map[string]interface{} {
	spec := sb.GetSpec()
	tmpl := spec.GetTemplate()

	container := map[string]interface{}{
		"name":  "agent",
		"image": tmpl.GetImage(),
		"env":   buildEnvList(spec.GetEnvironment(), tmpl.GetEnvironment()),
		"securityContext": map[string]interface{}{
			"capabilities": map[string]interface{}{
				"add": []interface{}{"SYS_ADMIN", "NET_ADMIN", "SYS_PTRACE", "SYSLOG"},
			},
		},
	}

	if res := tmpl.GetResources(); res != nil {
		container["resources"] = buildResources(res, spec.GetGpu())
	}

	podSpec := map[string]interface{}{
		"containers": []interface{}{container},
	}

	// Apply platform_config passthrough fields.
	if pc := tmpl.GetPlatformConfig(); pc != nil {
		fields := pc.GetFields()
		if rcn, ok := fields["runtime_class_name"]; ok {
			podSpec["runtimeClassName"] = rcn.GetStringValue()
		}
	}

	return map[string]interface{}{
		"podTemplate": map[string]interface{}{
			"metadata": map[string]interface{}{
				"labels": mergeMaps(tmpl.GetLabels(), map[string]string{
					labelSandboxID: sb.GetId(),
					labelManagedBy: "openshell",
					labelKagenti:   "agent",
				}),
			},
			"spec": podSpec,
		},
	}
}

// hasGPUCapacity checks whether any node in the cluster has nvidia.com/gpu
// allocatable.
func (d *Driver) hasGPUCapacity(ctx context.Context) (bool, error) {
	nodes, err := d.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	gpuResource := corev1.ResourceName("nvidia.com/gpu")
	for _, node := range nodes.Items {
		if alloc := node.Status.Allocatable; alloc != nil {
			if q, ok := alloc[gpuResource]; ok && !q.IsZero() {
				return true, nil
			}
		}
	}
	return false, nil
}
