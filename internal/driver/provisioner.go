package driver

import (
	"context"
	"fmt"
	"log/slog"

	pb "github.com/zanetworker/openshell-driver-openshift/gen/computev1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
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
)

// K8sProvisioner implements SandboxProvisioner using the Kubernetes API. It
// manages Sandbox CRDs in a single namespace.
type K8sProvisioner struct {
	dynamic   dynamic.Interface
	clientset kubernetes.Interface
	cfg       Config
	logger    *slog.Logger
}

// NewK8sProvisioner creates a K8sProvisioner with pre-built K8s clients.
func NewK8sProvisioner(
	dynClient dynamic.Interface,
	clientset kubernetes.Interface,
	cfg Config,
	logger *slog.Logger,
) *K8sProvisioner {
	return &K8sProvisioner{
		dynamic:   dynClient,
		clientset: clientset,
		cfg:       cfg,
		logger:    logger,
	}
}

// ValidateCreate checks whether the cluster can satisfy the sandbox request.
// Currently it verifies GPU capacity when the sandbox requests a GPU.
func (p *K8sProvisioner) ValidateCreate(ctx context.Context, sb *pb.DriverSandbox) error {
	if sb.GetSpec().GetGpu() {
		ok, err := p.HasGPUCapacity(ctx)
		if err != nil {
			return fmt.Errorf("check GPU capacity: %w", err)
		}
		if !ok {
			return fmt.Errorf("no nodes with nvidia.com/gpu allocatable in the cluster")
		}
	}
	return nil
}

// Create provisions a Sandbox CR in the target namespace.
func (p *K8sProvisioner) Create(ctx context.Context, sb *pb.DriverSandbox) error {
	spec := sb.GetSpec()
	tmpl := spec.GetTemplate()

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
				"namespace": p.cfg.Namespace,
				"labels":    labels,
			},
			"spec": p.buildSandboxSpec(sb),
		},
	}

	_, err := p.dynamic.Resource(sandboxGVR).
		Namespace(p.cfg.Namespace).
		Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create Sandbox CR %s: %w", sb.GetName(), err)
	}

	p.logger.Info("sandbox created",
		"name", sb.GetName(),
		"id", sb.GetId(),
		"gpu", spec.GetGpu())
	return nil
}

// Delete removes the Sandbox CR by name.
func (p *K8sProvisioner) Delete(ctx context.Context, name string) error {
	return p.dynamic.Resource(sandboxGVR).
		Namespace(p.cfg.Namespace).
		Delete(ctx, name, metav1.DeleteOptions{})
}

// Get retrieves a single Sandbox CR by name and converts it to a DriverSandbox.
func (p *K8sProvisioner) Get(ctx context.Context, name string) (*pb.DriverSandbox, error) {
	obj, err := p.dynamic.Resource(sandboxGVR).
		Namespace(p.cfg.Namespace).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return objToDriverSandbox(obj), nil
}

// List returns all Sandbox CRs managed by openshell in the target namespace.
func (p *K8sProvisioner) List(ctx context.Context) ([]*pb.DriverSandbox, error) {
	list, err := p.dynamic.Resource(sandboxGVR).
		Namespace(p.cfg.Namespace).
		List(ctx, metav1.ListOptions{
			LabelSelector: labelSandboxID,
		})
	if err != nil {
		return nil, err
	}

	sandboxes := make([]*pb.DriverSandbox, 0, len(list.Items))
	for i := range list.Items {
		sandboxes = append(sandboxes, objToDriverSandbox(&list.Items[i]))
	}
	return sandboxes, nil
}

// Watch starts a K8s watch on Sandbox CRs and returns a channel of WatchEvent
// values. The channel is closed when the underlying watcher stops or the
// context is cancelled.
func (p *K8sProvisioner) Watch(ctx context.Context) (<-chan WatchEvent, error) {
	watcher, err := p.dynamic.Resource(sandboxGVR).
		Namespace(p.cfg.Namespace).
		Watch(ctx, metav1.ListOptions{
			LabelSelector: labelSandboxID,
		})
	if err != nil {
		return nil, fmt.Errorf("start watcher: %w", err)
	}

	ch := make(chan WatchEvent)
	go func() {
		defer close(ch)
		defer watcher.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.ResultChan():
				if !ok {
					return
				}
				obj, isUnstructured := event.Object.(*unstructured.Unstructured)
				if !isUnstructured {
					continue
				}

				var evt WatchEvent
				switch event.Type {
				case watch.Added, watch.Modified:
					evt = WatchEvent{
						Type:    WatchEventUpdated,
						Sandbox: objToDriverSandbox(obj),
					}
				case watch.Deleted:
					evt = WatchEvent{
						Type:      WatchEventDeleted,
						SandboxID: obj.GetLabels()[labelSandboxID],
					}
				default:
					continue
				}

				select {
				case ch <- evt:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return ch, nil
}

// HasGPUCapacity checks whether any node in the cluster has nvidia.com/gpu
// allocatable.
func (p *K8sProvisioner) HasGPUCapacity(ctx context.Context) (bool, error) {
	nodes, err := p.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
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

// buildSandboxSpec constructs the Sandbox CR spec from driver-native messages.
// It injects the supervisor binary via an init container and configures the
// agent container to run the supervisor as its entrypoint.
func (p *K8sProvisioner) buildSandboxSpec(sb *pb.DriverSandbox) map[string]interface{} {
	spec := sb.GetSpec()
	tmpl := spec.GetTemplate()

	// Supervisor init container copies the binary into the shared volume.
	initContainer := map[string]interface{}{
		"name":    "supervisor-init",
		"image":   p.cfg.SupervisorImage,
		"command": []interface{}{"cp", p.cfg.SupervisorBinaryPath, p.cfg.SupervisorMountPath + "/"},
		"volumeMounts": []interface{}{
			map[string]interface{}{
				"name":      "supervisor-bin",
				"mountPath": p.cfg.SupervisorMountPath,
			},
		},
	}

	// Agent container runs the supervisor and mounts it read-only.
	container := map[string]interface{}{
		"name":    "agent",
		"image":   tmpl.GetImage(),
		"command": []interface{}{p.cfg.SupervisorMountPath + "/openshell-sandbox"},
		"env":     p.buildFullEnvList(sb, spec, tmpl),
		"securityContext": map[string]interface{}{
			"runAsUser": int64(0),
			"capabilities": map[string]interface{}{
				"add": []interface{}{"SYS_ADMIN", "NET_ADMIN", "SYS_PTRACE", "SYSLOG"},
			},
		},
		"volumeMounts": []interface{}{
			map[string]interface{}{
				"name":      "supervisor-bin",
				"mountPath": p.cfg.SupervisorMountPath,
				"readOnly":  true,
			},
		},
	}

	if res := tmpl.GetResources(); res != nil {
		container["resources"] = buildResources(res, spec.GetGpu())
	}

	podSpec := map[string]interface{}{
		"initContainers": []interface{}{initContainer},
		"containers":     []interface{}{container},
		"serviceAccountName": "openshell-sandbox",
		"volumes": []interface{}{
			map[string]interface{}{
				"name":     "supervisor-bin",
				"emptyDir": map[string]interface{}{},
			},
		},
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

func (p *K8sProvisioner) buildFullEnvList(
	sb *pb.DriverSandbox,
	spec *pb.DriverSandboxSpec,
	tmpl *pb.DriverSandboxTemplate,
) []interface{} {
	envList := buildEnvList(spec.GetEnvironment(), tmpl.GetEnvironment())

	gatewayEnv := map[string]string{
		"OPENSHELL_SANDBOX_ID": sb.GetId(),
		"OPENSHELL_SANDBOX":    sb.GetName(),
	}
	if p.cfg.GatewayEndpoint != "" {
		gatewayEnv["OPENSHELL_ENDPOINT"] = p.cfg.GatewayEndpoint
	}

	gatewayEnv["ANTHROPIC_BASE_URL"] = "https://inference.local/v1"
	gatewayEnv["OPENAI_BASE_URL"] = "https://inference.local/v1"

	for k, v := range gatewayEnv {
		envList = append(envList, map[string]interface{}{
			"name":  k,
			"value": v,
		})
	}

	return envList
}
