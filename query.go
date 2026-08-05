package main

import (
	"encoding/json"
	"log"
	"net/http"
)

// The vocabulary the caller already speaks. The agent accepts the existing terms
// rather than inventing its own, so changing where the data comes from does not
// reach the screen.
var granularitySeconds = map[string]int64{
	"minute": 60,
	"hour":   3600,
	"day":    86400,
}

type metricsQueryBody struct {
	Metric      string `json:"metric"`
	Namespace   string `json:"namespace"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	Granularity string `json:"granularity"`
}

type metricsQueryResponse struct {
	// The bucket size ACTUALLY used. `minute` always comes back as 300 here:
	// whoever draws the chart needs the real spacing to label the axis, and
	// guessing it from what was asked would produce a wrong axis.
	BucketSeconds int64        `json:"bucketSeconds"`
	Points        []queryPoint `json:"points"`
}

func newMetricsQueryHandler(store *metricStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var body metricsQueryBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if body.Metric == "" || body.Namespace == "" || body.Kind == "" || body.Name == "" {
			http.Error(w, "metric, namespace, kind and name are required", http.StatusBadRequest)
			return
		}
		if body.End <= body.Start {
			http.Error(w, "end must be greater than start (Unix seconds)", http.StatusBadRequest)
			return
		}

		step, ok := granularitySeconds[body.Granularity]
		if !ok {
			step = bucketSeconds
		}

		key := seriesKey{
			Metric:    body.Metric,
			Namespace: body.Namespace,
			Kind:      body.Kind,
			Name:      body.Name,
		}
		used, points := store.Query(key, body.Start, body.End, step)

		// An empty series is a legitimate answer, not an error: the workload may
		// be younger than the window asked for, or the agent may have started
		// recently. Telling "no data" apart from "could not ask" is the caller's
		// job, and it has the network failure to go on -- here, 200 with an empty
		// list is the truth.
		if points == nil {
			points = []queryPoint{}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(metricsQueryResponse{BucketSeconds: used, Points: points}); err != nil {
			log.Printf("[metrics-query] failed to encode the response: %v", err)
		}
	}
}
