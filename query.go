package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// The vocabulary the caller already speaks. The agent accepts the existing terms
// rather than inventing its own, so changing where the data comes from does not
// reach the screen.
var granularitySeconds = map[string]int64{
	"minute": 60,
	"hour":   3600,
	"day":    86400,
}

// Default step when the caller asks for a granularity this agent does not know.
// Five minutes is the coarsest of the three the product offers, so an unknown
// name degrades to fewer points rather than to a query heavy enough to hurt
// Prometheus.
const defaultStepSeconds int64 = 300

// Ceiling on points per answer. Prometheus refuses a range query above 11 000
// steps with an error the reader cannot act on ("exceeded maximum resolution");
// clamping here turns that into a coarser chart, which is the same thing the
// chart would show anyway at that width.
const maxPointsPerQuery int64 = 3000

type metricsQueryBody struct {
	Metric      string `json:"metric"`
	Namespace   string `json:"namespace"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	Granularity string `json:"granularity"`

	// The query to run, as declared in the resource's metrics.js, with
	// `{namespace}` / `{name}` / `{kind}` still in it. See renderPromQL for why
	// the substitution happens here and not on the way in.
	PromQL string `json:"promql"`
}

type metricsQueryResponse struct {
	// The bucket size ACTUALLY used. Whoever draws the chart needs the real
	// spacing to label the axis, and guessing it from what was asked would
	// produce a wrong axis.
	BucketSeconds int64        `json:"bucketSeconds"`
	Points        []queryPoint `json:"points"`
}

// renderPromQL fills the template from the catalogue with this resource's
// identity.
//
// The substitution is done HERE, and not by whoever wrote the query, because
// `namespace` and `name` come from the node's logical name in the diagram --
// free text, typed by a user. A node called `a"} or up{` interpolated raw would
// close the label matcher and append a second selector, and the chart would come
// back full of numbers belonging to something else entirely.
//
// Escaping follows the PromQL string rule: backslash first (so the escapes added
// next are not themselves escaped), then the double quote. Newlines cannot
// survive a label value either, so they go too.
func renderPromQL(template string, b metricsQueryBody) string {
	replacer := strings.NewReplacer(
		"{namespace}", escapePromQLValue(b.Namespace),
		"{name}", escapePromQLValue(b.Name),
		"{kind}", escapePromQLValue(b.Kind),
	)
	return replacer.Replace(template)
}

func escapePromQLValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}

func newMetricsQueryHandler(prom *prometheusClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// No Prometheus configured is a state of the deployment, not of the
		// request -- and it is the likeliest reason for this endpoint to be
		// unreachable, so it says so in full instead of returning an empty
		// series that would draw as "this workload used nothing".
		if prom == nil {
			http.Error(
				w,
				"this agent has no Prometheus configured: set PROMETHEUS_URL (the Prometheus URL field on the Struct8 Agent) and the chart starts answering",
				http.StatusServiceUnavailable,
			)
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
		// The agent carries no query of its own on purpose. A built-in fallback
		// would be a second copy of the catalogue, and the two would drift apart
		// without anything failing -- the chart would simply answer about
		// something else.
		if strings.TrimSpace(body.PromQL) == "" {
			http.Error(w, "promql is required: it comes from the resource's metrics.js", http.StatusBadRequest)
			return
		}
		if body.End <= body.Start {
			http.Error(w, "end must be greater than start (Unix seconds)", http.StatusBadRequest)
			return
		}

		step, ok := granularitySeconds[body.Granularity]
		if !ok {
			step = defaultStepSeconds
		}
		if span := body.End - body.Start; span/step > maxPointsPerQuery {
			step = span / maxPointsPerQuery
		}

		promQL := renderPromQL(body.PromQL, body)
		points, err := prom.QueryRange(r.Context(), promQL, body.Start, body.End, step)
		if err != nil {
			log.Printf("[metrics-query] %s %s/%s (%s): %v", body.Metric, body.Kind, body.Name, body.Namespace, err)

			// A query that came back with several series is a defect in the
			// catalogue entry, not a fault of this cluster -- and it is fixed in
			// a different place by a different person, so it gets its own status.
			if many, isMany := err.(*errTooManySeries); isMany {
				http.Error(w, many.Error(), http.StatusUnprocessableEntity)
				return
			}
			http.Error(w, "could not query Prometheus: "+err.Error(), http.StatusBadGateway)
			return
		}

		// An empty series is a legitimate answer, not an error: the workload may
		// be younger than the window asked for, or the metric may not be scraped
		// yet. Telling "no data" apart from "could not ask" is the caller's job,
		// and every failure above is a non-200 -- here, 200 with an empty list is
		// the truth.
		if points == nil {
			points = []queryPoint{}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(metricsQueryResponse{BucketSeconds: step, Points: points}); err != nil {
			log.Printf("[metrics-query] failed to encode the response: %v", err)
		}
	}
}
