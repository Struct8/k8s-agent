package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Only the kinds covered by the read-only ClusterRole (see README.md). A kind
// outside this list always comes back as "unknown_kind" instead of raising an
// RBAC error at the caller -- see resolveStatus.
var statusGVRs = map[string]schema.GroupVersionResource{
	"Pod":         {Group: "", Version: "v1", Resource: "pods"},
	"Service":     {Group: "", Version: "v1", Resource: "services"},
	"Deployment":  {Group: "apps", Version: "v1", Resource: "deployments"},
	"DaemonSet":   {Group: "apps", Version: "v1", Resource: "daemonsets"},
	"StatefulSet": {Group: "apps", Version: "v1", Resource: "statefulsets"},
	"ReplicaSet":  {Group: "apps", Version: "v1", Resource: "replicasets"},
	"Job":         {Group: "batch", Version: "v1", Resource: "jobs"},
	"CronJob":     {Group: "batch", Version: "v1", Resource: "cronjobs"},
}

type statusResourceQuery struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type statusRequestBody struct {
	Resources []statusResourceQuery `json:"resources"`
}

type statusResult struct {
	Deployed bool                   `json:"deployed"`
	Status   string                 `json:"status"`
	UID      string                 `json:"uid"`
	Outputs  map[string]interface{} `json:"outputs"`
}

func newStatusHandler(client dynamic.Interface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var body statusRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		results := make([]statusResult, len(body.Resources))
		for i, q := range body.Resources {
			results[i] = resolveStatus(ctx, client, q)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(results); err != nil {
			log.Printf("[status] failed to encode the response: %v", err)
		}
	}
}

func resolveStatus(ctx context.Context, client dynamic.Interface, q statusResourceQuery) statusResult {
	gvr, known := statusGVRs[q.Kind]
	if !known {
		return statusResult{Deployed: false, Status: "unknown_kind", Outputs: map[string]interface{}{}}
	}

	var obj *unstructured.Unstructured
	var err error
	if q.Namespace != "" {
		obj, err = client.Resource(gvr).Namespace(q.Namespace).Get(ctx, q.Name, metav1.GetOptions{})
	} else {
		obj, err = client.Resource(gvr).Get(ctx, q.Name, metav1.GetOptions{})
	}

	if err != nil {
		if apierrors.IsNotFound(err) {
			return statusResult{Deployed: false, Status: "", Outputs: map[string]interface{}{}}
		}
		log.Printf("[status] failed to query %s/%s (%s): %v", q.Kind, q.Name, q.Namespace, err)
		return statusResult{Deployed: false, Status: "error", Outputs: map[string]interface{}{}}
	}

	return summarize(q.Kind, obj)
}

// summarize reduces the raw apiserver object to the (deployed, status) pair
// the caller renders. Each kind carries its own notion of what "ready" means,
// which is why this is a switch rather than one generic rule.
func summarize(kind string, obj *unstructured.Unstructured) statusResult {
	uid := string(obj.GetUID())
	outputs := map[string]interface{}{}

	switch kind {
	case "Pod":
		phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
		outputs["phase"] = phase
		deployed := phase == "Running" || phase == "Succeeded"
		return statusResult{Deployed: deployed, Status: phase, UID: uid, Outputs: outputs}

	case "Deployment", "StatefulSet":
		desired, _, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
		ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
		outputs["desiredReplicas"] = desired
		outputs["readyReplicas"] = ready
		deployed := desired > 0 && ready >= desired
		status := readyStatusLabel(ready, desired)
		return statusResult{Deployed: deployed, Status: status, UID: uid, Outputs: outputs}

	case "DaemonSet":
		desired, _, _ := unstructured.NestedInt64(obj.Object, "status", "desiredNumberScheduled")
		ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "numberReady")
		outputs["desiredReplicas"] = desired
		outputs["readyReplicas"] = ready
		deployed := desired > 0 && ready >= desired
		status := readyStatusLabel(ready, desired)
		return statusResult{Deployed: deployed, Status: status, UID: uid, Outputs: outputs}

	case "ReplicaSet":
		desired, _, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
		ready, _, _ := unstructured.NestedInt64(obj.Object, "status", "readyReplicas")
		outputs["desiredReplicas"] = desired
		outputs["readyReplicas"] = ready
		deployed := desired > 0 && ready >= desired
		return statusResult{Deployed: deployed, Status: readyStatusLabel(ready, desired), UID: uid, Outputs: outputs}

	case "Job":
		succeeded, _, _ := unstructured.NestedInt64(obj.Object, "status", "succeeded")
		active, _, _ := unstructured.NestedInt64(obj.Object, "status", "active")
		failed, _, _ := unstructured.NestedInt64(obj.Object, "status", "failed")
		outputs["succeeded"] = succeeded
		outputs["active"] = active
		outputs["failed"] = failed
		switch {
		case succeeded > 0:
			return statusResult{Deployed: true, Status: "Succeeded", UID: uid, Outputs: outputs}
		case active > 0:
			return statusResult{Deployed: true, Status: "Running", UID: uid, Outputs: outputs}
		case failed > 0:
			return statusResult{Deployed: false, Status: "Failed", UID: uid, Outputs: outputs}
		default:
			return statusResult{Deployed: false, Status: "Pending", UID: uid, Outputs: outputs}
		}

	case "CronJob":
		// Não tem "replicas prontas" -- é um agendamento, não um workload
		// vivo. Existir no apiserver já é o único sinal de "deployed" que faz
		// sentido aqui (equivalente ao deployedStatus:'always' do frontend
		// pra tipos sem consulta real).
		return statusResult{Deployed: true, Status: "Scheduled", UID: uid, Outputs: outputs}

	case "Service":
		return statusResult{Deployed: true, Status: "Active", UID: uid, Outputs: outputs}

	default:
		return statusResult{Deployed: true, Status: "Present", UID: uid, Outputs: outputs}
	}
}

func readyStatusLabel(ready, desired int64) string {
	return strconv.FormatInt(ready, 10) + "/" + strconv.FormatInt(desired, 10)
}
