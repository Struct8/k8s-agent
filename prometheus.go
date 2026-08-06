package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The agent asks Prometheus; it does not keep a series of its own.
//
// Collecting, storing and pushing used to live here, and all three were the
// agent doing badly what a time-series database does well: retention capped at
// what fits in the Pod, one fixed bucket size, memory growing with the number
// of active series, and a per-workload attribution rule that had to be kept in
// step with the caller's. None of that is this process's job.
//
// What is left is the part only this process can do: it sits inside the
// cluster, so it can reach a Prometheus that is not published to the internet.

// A point on the chart. The field names are the wire contract with the Worker
// (`queryAgentMetrics` in k8sAgentApi.ts reads `bucket` and `value`), so they
// are not free to rename.
type queryPoint struct {
	Bucket int64   `json:"bucket"`
	Value  float64 `json:"value"`
}

type prometheusClient struct {
	baseURL string
	http    *http.Client
}

// newPrometheusClient returns nil when no address is configured. A nil client
// is a valid state, not a failure: an agent with no Prometheus still answers
// status, and the metrics endpoint says so in its own words (see
// newMetricsQueryHandler).
func newPrometheusClient(baseURL string) *prometheusClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}
	return &prometheusClient{
		baseURL: baseURL,
		// Shorter than the Worker's own 5 s timeout on this call, so a slow
		// Prometheus surfaces here -- with an address and a reason -- instead of
		// as an anonymous timeout one layer up.
		http: &http.Client{Timeout: 4 * time.Second},
	}
}

// The shape of /api/v1/query_range. Only the fields that are read are declared;
// Prometheus sends more.
type promRangeResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			// [ [ 1785975659.0, "0.42" ], ... ] -- the instant is a number and
			// the value is a STRING, because a float64 cannot carry NaN or the
			// full int64 range through JSON.
			Values [][2]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// errTooManySeries is what a query that did not aggregate looks like from here.
//
// Returning the first series instead would draw a chart labelled with one
// workload out of another workload's numbers -- wrong in the one way nobody
// checks, because the chart looks perfectly normal.
type errTooManySeries struct {
	count  int
	labels []string
}

func (e *errTooManySeries) Error() string {
	return fmt.Sprintf("the query returned %d series (%s); it has to aggregate down to one", e.count, strings.Join(e.labels, "; "))
}

// QueryRange runs a range query and returns its single series as chart points.
func (c *prometheusClient) QueryRange(ctx context.Context, promQL string, start, end, step int64) ([]queryPoint, error) {
	params := url.Values{}
	params.Set("query", promQL)
	params.Set("start", strconv.FormatInt(start, 10))
	params.Set("end", strconv.FormatInt(end, 10))
	params.Set("step", strconv.FormatInt(step, 10))

	endpoint := c.baseURL + "/api/v1/query_range?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("building the request to %s: %w", c.baseURL, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching Prometheus at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	// Prometheus answers 400/422 with a JSON body naming what is wrong with the
	// query -- which is the whole diagnosis, and is lost if only the status code
	// travels. Decoded before the status check for exactly that reason.
	var body promRangeResponse
	decodeErr := json.NewDecoder(resp.Body).Decode(&body)

	if resp.StatusCode != http.StatusOK {
		if decodeErr == nil && body.Error != "" {
			return nil, fmt.Errorf("Prometheus refused the query (%d): %s", resp.StatusCode, body.Error)
		}
		return nil, fmt.Errorf("Prometheus answered %d", resp.StatusCode)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("unreadable answer from Prometheus at %s: %w", c.baseURL, decodeErr)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("Prometheus answered status %q: %s", body.Status, body.Error)
	}

	// No series is a legitimate answer: the workload may be younger than the
	// window, or the metric may not be scraped yet. Distinguishing that from
	// "could not ask" is the caller's job, and every failure above is an error
	// while this one is an empty list.
	if len(body.Data.Result) == 0 {
		return []queryPoint{}, nil
	}
	if len(body.Data.Result) > 1 {
		labels := make([]string, 0, len(body.Data.Result))
		for i, r := range body.Data.Result {
			if i == 3 {
				labels = append(labels, "...")
				break
			}
			labels = append(labels, formatSeriesLabels(r.Metric))
		}
		return nil, &errTooManySeries{count: len(body.Data.Result), labels: labels}
	}

	raw := body.Data.Result[0].Values
	points := make([]queryPoint, 0, len(raw))
	for _, pair := range raw {
		var at float64
		if err := json.Unmarshal(pair[0], &at); err != nil {
			continue
		}
		var text string
		if err := json.Unmarshal(pair[1], &text); err != nil {
			continue
		}
		// NaN, +Inf and -Inf are legal Prometheus values and are not numbers a
		// chart can plot. Dropping the point leaves a gap, which is what they
		// mean; passing them through would produce invalid JSON.
		value, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		points = append(points, queryPoint{Bucket: int64(at), Value: value})
	}
	return points, nil
}

// formatSeriesLabels renders one series' label set for the error above, so the
// reader sees WHICH series came back and can tell what the query failed to
// aggregate over.
func formatSeriesLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return "{" + strings.Join(parts, ",") + "}"
}
