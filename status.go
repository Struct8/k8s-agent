package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// LEGACY PATH. Only used by a query that arrives WITHOUT an apiVersion -- an
// older caller, still in the air while the new one is not published. A query
// carrying an apiVersion resolves through the RESTMapper (see resolveGVR),
// which sees any CRD installed in the cluster without a new agent release.
//
// Do not add kinds here: a new kind arrives by declaring its apiVersion and
// kind at the caller, which is where the rest of the product already reads
// that information from.
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

// What resolveGVR needs from a RESTMapper. `Reset` is part of it because the
// mapping is cached and the cluster gains CRDs after the agent is up (every
// Argo CD sync can install one): without invalidating the cache, a new kind
// would answer "does not exist" until someone restarted the Pod.
// *restmapper.DeferredDiscoveryRESTMapper satisfies both methods.
type restMapper interface {
	RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error)
	Reset()
}

type statusResourceQuery struct {
	// Empty when the query comes from an older caller -- see statusGVRs.
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	// Empty when the query is a list.
	Name string `json:"name"`
	// Asks for a LIST instead of a Get. Explicit rather than inferred from an
	// empty Name, so that a malformed query still becomes a visible error
	// instead of sweeping a whole namespace by accident.
	List bool `json:"list"`
	// Narrows the list. Optional: without it the list is everything in the
	// requested scope -- that is how you ask about an operator whose Deployment
	// carries whatever name its chart chose.
	Selector string `json:"selector"`
}

// resolveGVR translates (apiVersion, kind) into the REST resource and reports
// whether it is namespaced.
//
// The apiVersion is what makes the answer UNAMBIGUOUS. Kind alone does not
// identify an object: two CRDs from different operators can declare the same
// Kind in different groups, and picking the wrong one would return another
// object's status with no error at all -- HTTP 200 over the wrong thing.
func resolveGVR(
	apiVersion, kind string,
	mapper restMapper,
) (schema.GroupVersionResource, bool, error) {
	if apiVersion == "" {
		gvr, known := statusGVRs[kind]
		if !known {
			return schema.GroupVersionResource{}, false, fmt.Errorf("kind %q has no apiVersion and is outside the legacy map", kind)
		}
		// All eight in the legacy map are namespaced.
		return gvr, true, nil
	}

	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, false, err
	}

	gk := schema.GroupKind{Group: gv.Group, Kind: kind}
	mapping, err := mapper.RESTMapping(gk, gv.Version)
	if err != nil && meta.IsNoMatchError(err) {
		// It may be a CRD installed after the last discovery. One more attempt
		// with a cleared cache; if it still does not resolve, then it is really
		// absent.
		mapper.Reset()
		mapping, err = mapper.RESTMapping(gk, gv.Version)
	}
	if err != nil {
		return schema.GroupVersionResource{}, false, err
	}

	return mapping.Resource, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
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

func newStatusHandler(client dynamic.Interface, mapper restMapper) http.HandlerFunc {
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
			results[i] = resolveStatus(ctx, client, mapper, q)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(results); err != nil {
			log.Printf("[status] failed to encode the response: %v", err)
		}
	}
}

func resolveStatus(
	ctx context.Context,
	client dynamic.Interface,
	mapper restMapper,
	q statusResourceQuery,
) statusResult {
	gvr, namespaced, err := resolveGVR(q.APIVersion, q.Kind, mapper)
	if err != nil {
		log.Printf("[status] kind %q (%s) did not resolve: %v", q.Kind, q.APIVersion, err)
		return statusResult{Deployed: false, Status: "unknown_kind", Outputs: map[string]interface{}{}}
	}

	// The scope comes from the cluster, not from what the caller sent. The
	// caller resolves a namespace by walking its own hierarchy, and a Namespace
	// drawn inside another group would arrive here with one filled in -- a
	// namespaced query against a cluster-scoped object finds nothing and raises
	// no clear error.
	namespace := q.Namespace
	if !namespaced {
		namespace = ""
	}

	// LIST query. A node on the caller's side is not always one object: a
	// Gateway API route becomes N HTTPRoutes, one per connected target, with
	// names built by the generator out of a partition that only exists there.
	// Asking by name would mean reproducing that rule on the other side of the
	// product, and a silent divergence between the two copies would report
	// "not deployed" over a perfectly live object.
	//
	// What answers instead is the label the generator stamps on every object it
	// emits. The agent lists and aggregates; the naming rule keeps a single
	// owner.
	//
	// With no selector, the list is the WHOLE namespace. That is how you ask
	// about an operator installed by Helm: the Deployment name comes from a
	// third-party chart's templates, changes between versions, and is not ours
	// to predict -- but the namespace is created by the release itself and holds
	// nothing else.
	if q.List || q.Name == "" {
		if !q.List {
			log.Printf("[status] query for %q has no name and did not ask for a list", q.Kind)
			return statusResult{Deployed: false, Status: "error", Outputs: map[string]interface{}{}}
		}
		var list *unstructured.UnstructuredList
		if namespace != "" {
			list, err = client.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: q.Selector})
		} else {
			list, err = client.Resource(gvr).List(ctx, metav1.ListOptions{LabelSelector: q.Selector})
		}
		if err != nil {
			log.Printf("[status] failed to list %s (%s, %s): %v", q.Kind, q.Namespace, q.Selector, err)
			return statusResult{Deployed: false, Status: "error", Outputs: map[string]interface{}{}}
		}
		return summarizeGroup(q.Kind, list.Items)
	}

	var obj *unstructured.Unstructured
	if namespace != "" {
		obj, err = client.Resource(gvr).Namespace(namespace).Get(ctx, q.Name, metav1.GetOptions{})
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

// summarizeGroup condenses the N objects that matched into a single result --
// which is what the caller renders as one node.
//
// "Deployed" here means ALL of them deployed, not most and not the first: a
// route with three targets where one has no resolved backend is broken for
// whoever arrives that way, and a lit node would lie to exactly that person.
// Each object's name goes into `outputs` so the panel can say WHICH one failed,
// which is the next question.
func summarizeGroup(kind string, items []unstructured.Unstructured) statusResult {
	outputs := map[string]interface{}{"matched": len(items)}

	// Nothing matched. Same rendering as NotFound, because the honest answer is
	// the same: there is nothing in the cluster that this node stands for.
	if len(items) == 0 {
		return statusResult{Deployed: false, Status: "", Outputs: outputs}
	}

	ready := 0
	details := make([]interface{}, 0, len(items))
	each := make([]statusResult, 0, len(items))

	for i := range items {
		r := summarize(kind, &items[i])
		if r.Deployed {
			ready++
		}
		each = append(each, r)
		details = append(details, map[string]interface{}{
			"name":     items[i].GetName(),
			"deployed": r.Deployed,
			"status":   r.Status,
		})
	}

	outputs["ready"] = ready
	outputs["objects"] = details

	// A single object is the common case (one route, one target). Passing its
	// own status through -- "Accepted", "BackendNotFound" -- says more than
	// "1/1".
	if len(items) == 1 {
		for k, v := range each[0].Outputs {
			if _, taken := outputs[k]; !taken {
				outputs[k] = v
			}
		}
		return statusResult{
			Deployed: each[0].Deployed,
			Status:   each[0].Status,
			UID:      each[0].UID,
			Outputs:  outputs,
		}
	}

	// With more than one there is no single UID for the node: they are several
	// objects.
	return statusResult{
		Deployed: ready == len(items),
		Status:   readyStatusLabel(int64(ready), int64(len(items))),
		Outputs:  outputs,
	}
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

	case "Gateway":
		// Existing is not the signal here. A Gateway applied with no controller
		// to claim it, or with a listener in conflict, sits in the apiserver
		// forever without ever receiving an address -- and the entrypoint
		// answers nothing. `Accepted` says a controller took ownership;
		// `Programmed` says the infrastructure behind it (the load balancer)
		// exists.
		conds := readConditions(obj.Object, "status", "conditions")
		for condType, c := range conds {
			outputs[condType] = c.Status
		}
		if addrs, found, _ := unstructured.NestedSlice(obj.Object, "status", "addresses"); found && len(addrs) > 0 {
			if m, ok := addrs[0].(map[string]interface{}); ok {
				if v, _ := m["value"].(string); v != "" {
					outputs["address"] = v
				}
			}
		}
		ok, label := evaluateConditions(conds, []string{"Accepted", "Programmed"}, "Programmed")
		return statusResult{Deployed: ok, Status: label, UID: uid, Outputs: outputs}

	case "Application":
		// Argo CD already answers the whole question: `health` is the verdict
		// over everything this Application deployed, and it comes from the
		// controller actually reconciling it. Existing is worth nothing here --
		// an applied Application is a request, not a result.
		health, _, _ := unstructured.NestedString(obj.Object, "status", "health", "status")
		sync, _, _ := unstructured.NestedString(obj.Object, "status", "sync", "status")
		outputs["health"] = health
		outputs["sync"] = sync
		if rev, found, _ := unstructured.NestedString(obj.Object, "status", "sync", "revision"); found && rev != "" {
			outputs["revision"] = rev
		}

		// Freshly created, before the first reconciliation.
		if health == "" {
			return statusResult{Deployed: false, Status: "Pending", UID: uid, Outputs: outputs}
		}

		// Only `Healthy` counts. `Progressing`, `Degraded`, `Missing` and
		// `Suspended` are all states where something is NOT serving, and each
		// names itself better than any label of ours would.
		//
		// `OutOfSync` deliberately does NOT darken the node: what is in the
		// cluster is healthy and serving; what diverged was Git. That is drift,
		// not an outage, and it goes to `outputs` for whoever wants to look.
		return statusResult{Deployed: health == "Healthy", Status: health, UID: uid, Outputs: outputs}

	case "HTTPRoute":
		// An HTTPRoute's status is PER PARENT: the same route can be accepted by
		// one Gateway and refused by another, and each Gateway writes its own
		// block.
		parents, _, _ := unstructured.NestedSlice(obj.Object, "status", "parents")
		outputs["parents"] = len(parents)

		// An empty list is this type's most expensive failure to find: the
		// object was applied, the apply passed, Argo CD synced green, and no
		// controller claimed it -- a parentRef pointing at another Gateway, a
		// namespace outside the listener's `allowedRoutes`, or the controller
		// being down. At the entrypoint that is a 404 over a route that exists.
		if len(parents) == 0 {
			return statusResult{Deployed: false, Status: "NotAttached", UID: uid, Outputs: outputs}
		}

		for _, p := range parents {
			m, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			conds := readConditions(m, "conditions")
			// `ResolvedRefs` is what separates "the route is up" from "the route
			// is up pointing at a Service that does not exist" -- both apply
			// clean, and only the second returns 503.
			ready, label := evaluateConditions(conds, []string{"Accepted", "ResolvedRefs"}, "Accepted")
			if !ready {
				return statusResult{Deployed: false, Status: label, UID: uid, Outputs: outputs}
			}
		}
		return statusResult{Deployed: true, Status: "Accepted", UID: uid, Outputs: outputs}

	default:
		return statusResult{Deployed: true, Status: "Present", UID: uid, Outputs: outputs}
	}
}

func readyStatusLabel(ready, desired int64) string {
	return strconv.FormatInt(ready, 10) + "/" + strconv.FormatInt(desired, 10)
}

type condition struct {
	Status string
	Reason string
}

// readConditions indexes by `type` a list of conditions in the apiserver's
// standard shape, at the given path.
func readConditions(root map[string]interface{}, path ...string) map[string]condition {
	out := map[string]condition{}
	raw, _, _ := unstructured.NestedSlice(root, path...)
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _ := m["type"].(string)
		if condType == "" {
			continue
		}
		st, _ := m["status"].(string)
		reason, _ := m["reason"].(string)
		out[condType] = condition{Status: st, Reason: reason}
	}
	return out
}

// evaluateConditions requires ALL the listed conditions to be "True", and
// returns the label that reaches the screen.
//
// A missing condition is "Pending", not a failure: a freshly applied object has
// not been visited by its controller yet, and calling that an error would make
// every apply flash red for a few seconds.
//
// When a condition is False, the label is its `reason` -- "BackendNotFound",
// "RefNotPermitted", "NoValidListeners" name the cause and are the text
// Kubernetes already publishes. "False" on its own says nothing.
func evaluateConditions(conds map[string]condition, required []string, okLabel string) (bool, string) {
	for _, condType := range required {
		c, present := conds[condType]
		if !present {
			return false, "Pending"
		}
		if c.Status != "True" {
			if c.Reason != "" {
				return false, c.Reason
			}
			return false, "Not" + condType
		}
	}
	return true, okLabel
}
