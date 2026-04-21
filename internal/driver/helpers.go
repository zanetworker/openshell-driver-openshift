package driver

import (
	"fmt"
	"strconv"

	pb "github.com/zanetworker/openshell-driver-openshift/gen/computev1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// objToDriverSandbox converts a Kubernetes unstructured Sandbox CR into a
// DriverSandbox proto message by reading well-known fields from the object.
func objToDriverSandbox(obj *unstructured.Unstructured) *pb.DriverSandbox {
	labels := obj.GetLabels()
	sandboxID := labels["openshell.ai/sandbox-id"]

	sb := &pb.DriverSandbox{
		Id:        sandboxID,
		Name:      obj.GetName(),
		Namespace: obj.GetNamespace(),
	}

	// Extract status fields if present. NestedMap returns an error only for
	// type mismatches (e.g., status is a string), which we treat as absent.
	status, found, err := unstructured.NestedMap(obj.Object, "status")
	if err == nil && found {
		sb.Status = statusFromMap(status)
	}

	// Mark as deleting if a deletion timestamp exists.
	if obj.GetDeletionTimestamp() != nil && sb.Status != nil {
		sb.Status.Deleting = true
	}

	return sb
}

// statusFromMap extracts DriverSandboxStatus fields from a status map.
func statusFromMap(status map[string]interface{}) *pb.DriverSandboxStatus {
	ds := &pb.DriverSandboxStatus{}

	if v, ok := status["sandboxName"].(string); ok {
		ds.SandboxName = v
	}
	if v, ok := status["agentPod"].(string); ok {
		ds.InstanceId = v
	}
	if v, ok := status["agentFd"].(string); ok {
		ds.AgentFd = v
	}
	if v, ok := status["sandboxFd"].(string); ok {
		ds.SandboxFd = v
	}

	// Parse conditions array.
	if conds, ok := status["conditions"].([]interface{}); ok {
		for _, c := range conds {
			cmap, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			ds.Conditions = append(ds.Conditions, &pb.DriverCondition{
				Type:               getString(cmap, "type"),
				Status:             getString(cmap, "status"),
				Reason:             getString(cmap, "reason"),
				Message:            getString(cmap, "message"),
				LastTransitionTime: getString(cmap, "lastTransitionTime"),
			})
		}
	}

	return ds
}

// buildEnvList merges spec-level and template-level environment maps into a
// list of {name, value} maps suitable for a Kubernetes container spec.
func buildEnvList(specEnv, tmplEnv map[string]string) []interface{} {
	merged := make(map[string]string)
	for k, v := range tmplEnv {
		merged[k] = v
	}
	for k, v := range specEnv {
		merged[k] = v
	}

	var envList []interface{}
	for k, v := range merged {
		envList = append(envList, map[string]interface{}{
			"name":  k,
			"value": v,
		})
	}
	return envList
}

// buildResources converts DriverResourceRequirements and a GPU flag into a
// Kubernetes container resources map.
func buildResources(res *pb.DriverResourceRequirements, gpu bool) map[string]interface{} {
	requests := map[string]interface{}{}
	limits := map[string]interface{}{}

	if res.GetCpuRequest() != "" {
		requests["cpu"] = res.GetCpuRequest()
	}
	if res.GetMemoryRequest() != "" {
		requests["memory"] = res.GetMemoryRequest()
	}
	if res.GetCpuLimit() != "" {
		limits["cpu"] = res.GetCpuLimit()
	}
	if res.GetMemoryLimit() != "" {
		limits["memory"] = res.GetMemoryLimit()
	}
	if gpu {
		limits["nvidia.com/gpu"] = "1"
	}

	result := map[string]interface{}{}
	if len(requests) > 0 {
		result["requests"] = requests
	}
	if len(limits) > 0 {
		result["limits"] = limits
	}
	return result
}

// mergeMaps merges two string maps. Values in b override values in a.
func mergeMaps(a, b map[string]string) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range a {
		result[k] = v
	}
	for k, v := range b {
		result[k] = v
	}
	return result
}

// getString safely extracts a string from a map.
func getString(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", val)
	}
}
