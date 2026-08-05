package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var podMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}

type metricPoint struct {
	Metric    string  `json:"metric"`
	Namespace string  `json:"namespace"`
	Kind      string  `json:"kind"`
	Name      string  `json:"name"`
	Value     float64 `json:"value"`
}

// Chave de agregação: um workload por namespace.
type workloadKey struct {
	Namespace string
	Kind      string
	Name      string
}

type workloadUsage struct {
	CPUCores float64
	MemMiB   float64
}

// collectWorkloadMetrics lê metrics.k8s.io (servido pelo metrics-server já
// instalado no cluster do cliente, ver README.md) -- é o substituto direto do
// scrape do Prometheus: CPU em cores fracionários, memória em MiB.
//
// Usage is summed per CONTAINER within a Pod, then per POD within the owning
// workload (see owners.go). The point that gets sent is the workload total,
// which is what a chart asks for -- one point per Pod would never match a query
// for "Deployment#<name>".
//
// Visible consequence of that sum: during a rollout the Pods of the previous
// ReplicaSet are still running and still counted, so the value rises until the
// rollout finishes. That is the correct answer to "what does this workload
// consume right now".
func collectWorkloadMetrics(ctx context.Context, client dynamic.Interface, resolver *ownerResolver) ([]metricPoint, int, error) {
	list, err := client.Resource(podMetricsGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("listing metrics.k8s.io/pods: %w", err)
	}

	keys := make([]objectKey, 0, len(list.Items))
	perPod := make(map[objectKey]workloadUsage, len(list.Items))

	for _, item := range list.Items {
		key := objectKey{Namespace: item.GetNamespace(), Name: item.GetName()}

		containers, _, _ := unstructured.NestedSlice(item.Object, "containers")
		var cpuNanos, memBytes int64
		for _, c := range containers {
			container, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			cpuStr, _, _ := unstructured.NestedString(container, "usage", "cpu")
			memStr, _, _ := unstructured.NestedString(container, "usage", "memory")

			if q, err := resource.ParseQuantity(cpuStr); err == nil {
				cpuNanos += q.ScaledValue(resource.Nano)
			}
			if q, err := resource.ParseQuantity(memStr); err == nil {
				memBytes += q.Value()
			}
		}

		keys = append(keys, key)
		perPod[key] = workloadUsage{
			CPUCores: float64(cpuNanos) / 1e9,
			MemMiB:   float64(memBytes) / (1024 * 1024),
		}
	}

	owners, unresolved, err := resolver.Resolve(ctx, keys)
	if err != nil {
		return nil, 0, fmt.Errorf("resolving Pod owners: %w", err)
	}

	totals := make(map[workloadKey]*workloadUsage, len(owners))
	for key, usage := range perPod {
		owner, ok := owners[key]
		if !ok {
			continue
		}
		agg := workloadKey{Namespace: key.Namespace, Kind: owner.Kind, Name: owner.Name}
		acc, ok := totals[agg]
		if !ok {
			acc = &workloadUsage{}
			totals[agg] = acc
		}
		acc.CPUCores += usage.CPUCores
		acc.MemMiB += usage.MemMiB
	}

	points := make([]metricPoint, 0, len(totals)*2)
	for agg, usage := range totals {
		points = append(points,
			metricPoint{Metric: "cpu", Namespace: agg.Namespace, Kind: agg.Kind, Name: agg.Name, Value: usage.CPUCores},
			metricPoint{Metric: "memory", Namespace: agg.Namespace, Kind: agg.Kind, Name: agg.Name, Value: usage.MemMiB},
		)
	}
	return points, len(unresolved), nil
}

// Mirrors the patterns the receiving API enforces on ingest. Kept identical on
// purpose: a point the API would reject is dropped here, with a log line,
// rather than crossing the network to be rejected there.
var (
	metricFieldPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)
	namespacePattern   = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,63}$`)
	objectNamePattern  = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,253}$`)
)

// sanitizePoints tira do lote o que não passaria na validação do Worker.
//
// Nome NÃO é truncado: um nome cortado não bate com o que o gráfico consulta, e
// gravaria uma série que parece boa e nunca é lida. Descartar com log é
// honesto; truncar seria mentir em silêncio.
func sanitizePoints(points []metricPoint) ([]metricPoint, []string) {
	kept := points[:0:0]
	var dropped []string
	for _, p := range points {
		if metricFieldPattern.MatchString(p.Metric) &&
			namespacePattern.MatchString(p.Namespace) &&
			metricFieldPattern.MatchString(p.Kind) &&
			objectNamePattern.MatchString(p.Name) {
			kept = append(kept, p)
			continue
		}
		dropped = append(dropped, fmt.Sprintf("%s/%s#%s", p.Namespace, p.Kind, p.Name))
	}
	return kept, dropped
}

type ingestRequestBody struct {
	Points []metricPoint `json:"points"`
}

// The ceiling the receiving API enforces, which in turn comes from the limit of
// the time-series backend it writes to: 250 points per request.
//
// This was 500 on both sides. A batch passed validation here and at the
// receiver, then hit the platform ceiling at the end of the line, where there is
// nobody left to report it -- points discarded with no visible error. A large
// cluster easily exceeds this in a single collection, so the push is always
// batched; the batch just fits now.
const maxPointsPerRequest = 250

// pushMetrics tenta TODOS os lotes, mesmo depois de um falhar.
//
// Parar no primeiro erro fazia um lote ruim levar junto todos os seguintes --
// e como a coleta é sempre a mesma, o mesmo lote falhava a cada ciclo e as
// métricas dos workloads que vinham depois dele nunca chegavam.
func pushMetrics(ctx context.Context, httpClient *http.Client, cfg Config, points []metricPoint) error {
	var errs []error
	for start := 0; start < len(points); start += maxPointsPerRequest {
		end := start + maxPointsPerRequest
		if end > len(points) {
			end = len(points)
		}
		if err := pushBatch(ctx, httpClient, cfg, points[start:end]); err != nil {
			errs = append(errs, fmt.Errorf("batch [%d:%d]: %w", start, end, err))
		}
	}
	return errors.Join(errs...)
}

func pushBatch(ctx context.Context, httpClient *http.Client, cfg Config, batch []metricPoint) error {
	body, err := json.Marshal(ingestRequestBody{Points: batch})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.WorkerBaseURL+"/v1/k8s-events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.ClusterAPIKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("worker responded %d: %s", resp.StatusCode, string(detail))
	}

	// O Worker grava o que dá e informa o que descartou. Sem isto, um ponto
	// recusado do lado de lá some sem deixar rastro nenhum.
	var ack struct {
		Skipped int `json:"skipped"`
	}
	if raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096)); err == nil {
		if json.Unmarshal(raw, &ack) == nil && ack.Skipped > 0 {
			log.Printf("[metrics] worker discarded %d point(s) from the batch", ack.Skipped)
		}
	}
	return nil
}
