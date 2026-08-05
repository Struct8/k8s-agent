package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The image a workload is RUNNING, read back off the live object.
//
// It is the only reading in the whole round trip that can contradict the
// diagram, and that is the point: the diagram holds the image that was asked
// for. A tag that moved, a layer already cached on the node, or a Pod that was
// never replaced all leave the two disagreeing, and none of them reports an
// error anywhere -- the agent keeps answering every request correctly, out of
// old code.

func workload(kind string, images ...string) *unstructured.Unstructured {
	containers := make([]interface{}, 0, len(images))
	for i, img := range images {
		containers = append(containers, map[string]interface{}{
			"name":  string(rune('a' + i)),
			"image": img,
		})
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"kind":     kind,
		"metadata": map[string]interface{}{"name": "x", "namespace": "struct8-agent", "uid": "u-1"},
		"spec": map[string]interface{}{
			"replicas": int64(1),
			"template": map[string]interface{}{
				"spec": map[string]interface{}{"containers": containers},
			},
		},
		"status": map[string]interface{}{"readyReplicas": int64(1)},
	}}
}

func TestWorkloadReportsRunningImage(t *testing.T) {
	for _, kind := range []string{"Deployment", "StatefulSet", "ReplicaSet"} {
		r := summarize(kind, workload(kind, "ghcr.io/struct8/k8s-agent:0.3.0"))
		if got := r.Outputs["image"]; got != "ghcr.io/struct8/k8s-agent:0.3.0" {
			t.Errorf("%s: image = %v, want the image from the Pod template", kind, got)
		}
		// The reading must not disturb what the node already showed.
		if !r.Deployed || r.Status != "1/1" {
			t.Errorf("%s: deployed=%v status=%q, want true and \"1/1\"", kind, r.Deployed, r.Status)
		}
	}
}

// A sidecar is as much part of "what is running" as the main container, and
// picking one of them would be picking arbitrarily.
func TestSeveralContainersAreAllReported(t *testing.T) {
	r := summarize("Deployment", workload("Deployment", "app:1.0", "sidecar:2.0"))
	if got := r.Outputs["image"]; got != "app:1.0, sidecar:2.0" {
		t.Errorf("image = %v, want both containers", got)
	}
}

// A kind with no Pod template must not grow an empty key: a panel showing
// "image:" with nothing after it reads as "no image", which is a different
// claim from "this kind has none".
func TestKindWithoutPodTemplateHasNoImageKey(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"kind":     "Service",
		"metadata": map[string]interface{}{"name": "x", "uid": "u-2"},
	}}
	r := summarize("Service", obj)
	if _, present := r.Outputs["image"]; present {
		t.Errorf("outputs carries an \"image\" key for a kind that has no Pod template")
	}
}
