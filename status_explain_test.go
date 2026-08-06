package main

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// An empty match has two opposite causes that render identically as a faded
// node: nothing was emitted for this node (the answer is in the diagram -- no
// enabled target), or something was emitted without the label (the answer is in
// Git -- that state was never pushed). These tests fix the readings that tell
// them apart.

var httpRouteGVR = schema.GroupVersionResource{
	Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes",
}

func httpRoute(name, namespace, origin string) *unstructured.Unstructured {
	labels := map[string]interface{}{}
	if origin != "" {
		labels[originLabel] = origin
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata": map[string]interface{}{
			"name": name, "namespace": namespace, "labels": labels,
		},
	}}
}

func fakeClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{httpRouteGVR: "HTTPRouteList"},
		objs...,
	)
}

// The state was pushed and other nodes are stamped -- so nothing was emitted
// for THIS node. Reading `otherOrigins` sends the reader back to the diagram.
func TestEmptyMatchNamesTheOriginsThatAreThere(t *testing.T) {
	client := fakeClient(
		httpRoute("games-kuma", "gamestest", "games"),
		httpRoute("games-supermario", "gamestest", "games"),
	)
	outputs := map[string]interface{}{}
	explainEmptyMatch(context.Background(), client, httpRouteGVR, "gamestest", outputs)

	if outputs["ofThisKindHere"] != 2 {
		t.Errorf("ofThisKindHere = %v, want 2", outputs["ofThisKindHere"])
	}
	if got := outputs["otherOrigins"]; !reflect.DeepEqual(got, []string{"games"}) {
		t.Errorf("otherOrigins = %v, want [games]", got)
	}
	if _, present := outputs["withoutOriginLabel"]; present {
		t.Errorf("everything here is stamped; reporting unstamped objects would be false")
	}
}

// Objects are there and none carries the label -- the state predates it and has
// not been pushed. This is the reading that says "go to Git", and getting it
// wrong sends the reader to the other half of the product.
func TestEmptyMatchCountsTheUnstamped(t *testing.T) {
	client := fakeClient(
		httpRoute("route2-argo1", "infra", ""),
		httpRoute("route2-struct8-agent", "infra", ""),
	)
	outputs := map[string]interface{}{}
	explainEmptyMatch(context.Background(), client, httpRouteGVR, "infra", outputs)

	if outputs["ofThisKindHere"] != 2 {
		t.Errorf("ofThisKindHere = %v, want 2", outputs["ofThisKindHere"])
	}
	if outputs["withoutOriginLabel"] != 2 {
		t.Errorf("withoutOriginLabel = %v, want 2", outputs["withoutOriginLabel"])
	}
	if _, present := outputs["otherOrigins"]; present {
		t.Errorf("no object carries the label; listing origins would invent one")
	}
}

// Nothing of the kind in the namespace at all. The count still has to be
// reported: "0 here" is a different statement from "the agent did not look".
func TestEmptyNamespaceStillReportsZero(t *testing.T) {
	outputs := map[string]interface{}{}
	explainEmptyMatch(context.Background(), fakeClient(), httpRouteGVR, "gamestest", outputs)

	if outputs["ofThisKindHere"] != 0 {
		t.Errorf("ofThisKindHere = %v, want 0", outputs["ofThisKindHere"])
	}
}

// End to end through the handler's own path, because the two tests above call
// explainEmptyMatch directly and would keep passing if the call site were
// removed -- the explanation would silently stop reaching anyone.
func TestEmptyMatchExplanationReachesTheResult(t *testing.T) {
	gv := schema.GroupVersion{Group: "gateway.networking.k8s.io", Version: "v1"}
	real := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	real.Add(gv.WithKind("HTTPRoute"), meta.RESTScopeNamespace)

	client := fakeClient(httpRoute("games-kuma", "gamestest", "games"))
	q := statusResourceQuery{
		APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute",
		Namespace: "gamestest", List: true, Selector: originLabel + "=wp-redirect",
	}

	res := resolveStatus(context.Background(), client, &fakeMapper{real: real}, q)

	if res.Deployed {
		t.Errorf("nothing matched; the node must not be reported as deployed")
	}
	if res.Outputs["matched"] != 0 {
		t.Errorf("matched = %v, want 0", res.Outputs["matched"])
	}
	if res.Outputs["ofThisKindHere"] != 1 {
		t.Errorf("the explanation did not reach the result (ofThisKindHere = %v)", res.Outputs["ofThisKindHere"])
	}
	if got := res.Outputs["otherOrigins"]; !reflect.DeepEqual(got, []string{"games"}) {
		t.Errorf("otherOrigins = %v, want [games]", got)
	}
	// Every answer carries what was asked, including this one.
	want := "HTTPRoute (gateway.networking.k8s.io/v1) in gamestest where " + originLabel + "=wp-redirect"
	if res.Outputs["query"] != want {
		t.Errorf("query = %v, want %q", res.Outputs["query"], want)
	}
}

// A query that DID match must not carry an explanation: "0 of this kind here"
// next to a healthy node reads as a contradiction.
func TestMatchedQueryCarriesNoExplanation(t *testing.T) {
	gv := schema.GroupVersion{Group: "gateway.networking.k8s.io", Version: "v1"}
	real := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	real.Add(gv.WithKind("HTTPRoute"), meta.RESTScopeNamespace)

	client := fakeClient(httpRoute("games-kuma", "gamestest", "games"))
	q := statusResourceQuery{
		APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute",
		Namespace: "gamestest", List: true, Selector: originLabel + "=games",
	}

	res := resolveStatus(context.Background(), client, &fakeMapper{real: real}, q)

	if res.Outputs["matched"] != 1 {
		t.Fatalf("matched = %v, want 1", res.Outputs["matched"])
	}
	for _, key := range []string{"ofThisKindHere", "otherOrigins", "withoutOriginLabel"} {
		if _, present := res.Outputs[key]; present {
			t.Errorf("%q is present on a query that matched", key)
		}
	}
}

func TestDescribeQuerySaysWhatWasAsked(t *testing.T) {
	cases := []struct {
		name string
		q    statusResourceQuery
		want string
	}{
		{
			"by label in a namespace",
			statusResourceQuery{
				APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute",
				Namespace: "gamestest", List: true, Selector: "struct8.io/origin=wp-redirect",
			},
			"HTTPRoute (gateway.networking.k8s.io/v1) in gamestest where struct8.io/origin=wp-redirect",
		},
		{
			"by label across the cluster",
			statusResourceQuery{
				APIVersion: "argoproj.io/v1alpha1", Kind: "Application",
				List: true, Selector: "struct8.io/origin=keycloak",
			},
			"Application (argoproj.io/v1alpha1) cluster-wide where struct8.io/origin=keycloak",
		},
		{
			"the whole namespace, for an operator installed by Helm",
			statusResourceQuery{
				APIVersion: "apps/v1", Kind: "Deployment",
				Namespace: "kong-operator-system", List: true,
			},
			"Deployment (apps/v1) in kong-operator-system (every one in it)",
		},
		{
			"by name",
			statusResourceQuery{
				APIVersion: "apps/v1", Kind: "Deployment",
				Namespace: "struct8-agent", Name: "struct8-agent",
			},
			"Deployment (apps/v1) named struct8-agent in struct8-agent",
		},
	}

	for _, c := range cases {
		if got := describeQuery(c.q); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
