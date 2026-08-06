package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The agent stopped collecting and storing series and now forwards the chart's
// question to Prometheus. What these tests hold in place is the part that
// cannot be seen from the screen: an empty answer is not a failure, a failure is
// not an empty answer, and the identity of the resource never reaches PromQL as
// executable text.

// fakePrometheus stands in for /api/v1/query_range, recording what it was asked.
type fakePrometheus struct {
	server   *httptest.Server
	lastQ    string
	lastStep string
	status   int
	body     string
	calls    int
}

func newFakePrometheus(body string) *fakePrometheus {
	f := &fakePrometheus{status: http.StatusOK, body: body}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		f.lastQ = r.URL.Query().Get("query")
		f.lastStep = r.URL.Query().Get("step")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	return f
}

func (f *fakePrometheus) close() { f.server.Close() }

const oneSeries = `{"status":"success","data":{"resultType":"matrix","result":[
  {"metric":{"namespace":"gamestest"},"values":[[1785975600,"0.25"],[1785975900,"0.5"]]}
]}}`

const noSeries = `{"status":"success","data":{"resultType":"matrix","result":[]}}`

const twoSeries = `{"status":"success","data":{"resultType":"matrix","result":[
  {"metric":{"pod":"kuma-a"},"values":[[1785975600,"1"]]},
  {"metric":{"pod":"kuma-b"},"values":[[1785975600,"2"]]}
]}}`

func askMetrics(t *testing.T, prom *prometheusClient, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("could not build the request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/metrics-query", strings.NewReader(string(payload)))
	rec := httptest.NewRecorder()
	newMetricsQueryHandler(prom)(rec, req)
	return rec
}

func validBody() map[string]interface{} {
	return map[string]interface{}{
		"metric":      "cpu",
		"namespace":   "gamestest",
		"kind":        "Deployment",
		"name":        "kuma",
		"start":       1785975600,
		"end":         1785979200,
		"granularity": "minute",
		"promql":      `sum(rate(container_cpu_usage_seconds_total{namespace="{namespace}",pod=~"{name}-.*"}[5m]))`,
	}
}

func TestSeriesBecomesPointsInOrder(t *testing.T) {
	fake := newFakePrometheus(oneSeries)
	defer fake.close()

	rec := askMetrics(t, newPrometheusClient(fake.server.URL), validBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var got metricsQueryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unreadable answer: %v", err)
	}
	want := []queryPoint{{Bucket: 1785975600, Value: 0.25}, {Bucket: 1785975900, Value: 0.5}}
	if len(got.Points) != len(want) {
		t.Fatalf("got %d points, want %d", len(got.Points), len(want))
	}
	for i := range want {
		if got.Points[i] != want[i] {
			t.Errorf("point %d = %+v, want %+v", i, got.Points[i], want[i])
		}
	}
	// The chart labels its axis with this; guessing it from the granularity
	// asked for would put the points at the wrong instants.
	if got.BucketSeconds != 60 {
		t.Errorf("bucketSeconds = %d, want 60", got.BucketSeconds)
	}
}

// An empty answer is a fact about the cluster, not a fault: the workload may be
// younger than the window, or the metric may not be scraped. It has to arrive as
// 200 so the caller can draw an empty state rather than an error.
func TestNoSeriesIsAnAnswerNotAFailure(t *testing.T) {
	fake := newFakePrometheus(noSeries)
	defer fake.close()

	rec := askMetrics(t, newPrometheusClient(fake.server.URL), validBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"points":[]`) {
		t.Errorf("body = %s, want an empty points list (not null: the caller iterates it)", body)
	}
}

// A query that did not aggregate is a defect in the resource's metrics.js, and
// it is fixed somewhere else by someone else -- so it gets its own status
// instead of the 502 that means "this cluster could not answer".
func TestSeveralSeriesIsRejectedInsteadOfPickingOne(t *testing.T) {
	fake := newFakePrometheus(twoSeries)
	defer fake.close()

	rec := askMetrics(t, newPrometheusClient(fake.server.URL), validBody())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body: %s)", rec.Code, rec.Body.String())
	}
	// The label sets have to reach the reader: they are what says WHAT the query
	// failed to aggregate over.
	if body := rec.Body.String(); !strings.Contains(body, "pod=kuma-a") {
		t.Errorf("body = %s, want the label sets that came back", body)
	}
}

// namespace and name come from the node's logical name in the diagram -- free
// text. Interpolated raw, this one closes the matcher and appends a second
// selector, and the chart comes back full of numbers belonging to something
// else, with nothing reporting an error.
func TestResourceNameCannotRewriteTheQuery(t *testing.T) {
	fake := newFakePrometheus(oneSeries)
	defer fake.close()

	body := validBody()
	body["name"] = `a"} or up{`
	askMetrics(t, newPrometheusClient(fake.server.URL), body)

	if strings.Contains(fake.lastQ, `or up{`) && !strings.Contains(fake.lastQ, `\"`) {
		t.Fatalf("the name reached PromQL unescaped: %s", fake.lastQ)
	}
	if !strings.Contains(fake.lastQ, `a\"} or up{`) {
		t.Errorf("query = %s, want the name escaped and kept as a literal", fake.lastQ)
	}
}

func TestBackslashInTheNameIsEscapedFirst(t *testing.T) {
	fake := newFakePrometheus(oneSeries)
	defer fake.close()

	body := validBody()
	body["name"] = `a\"b`
	askMetrics(t, newPrometheusClient(fake.server.URL), body)

	// Escaping the quote before the backslash would produce `a\\"b`, closing the
	// string one character early.
	if !strings.Contains(fake.lastQ, `a\\\"b`) {
		t.Errorf("query = %s, want the backslash escaped before the quote", fake.lastQ)
	}
}

// No Prometheus configured is a state of the deployment, and the likeliest
// reason for this endpoint to be silent. Answering with an empty series would
// draw as "this workload used nothing", which is a claim about the cluster.
func TestNoPrometheusConfiguredSaysSo(t *testing.T) {
	rec := askMetrics(t, newPrometheusClient("  "), validBody())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "PROMETHEUS_URL") {
		t.Errorf("body = %s, want the name of what is missing", rec.Body.String())
	}
}

// The agent carries no query of its own: a built-in fallback would be a second
// copy of the catalogue, and the two would drift apart without anything failing.
func TestMissingPromQLIsRefused(t *testing.T) {
	fake := newFakePrometheus(oneSeries)
	defer fake.close()

	body := validBody()
	delete(body, "promql")
	rec := askMetrics(t, newPrometheusClient(fake.server.URL), body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if fake.calls != 0 {
		t.Errorf("Prometheus was queried %d time(s) for a request with no query", fake.calls)
	}
}

// A chart about the whole cluster filters by nothing, so there is no namespace,
// no name and no kind to send -- and until v0.5.0 that was refused, which made
// "how much CPU is in use across everything" impossible to ask for.
func TestClusterWideQueryNeedsNoIdentity(t *testing.T) {
	fake := newFakePrometheus(oneSeries)
	defer fake.close()

	rec := askMetrics(t, newPrometheusClient(fake.server.URL), map[string]interface{}{
		"metric":      "cpu",
		"start":       1785975600,
		"end":         1785979200,
		"granularity": "minute",
		"promql":      `sum(rate(container_cpu_usage_seconds_total{container!=""}[5m]))`,
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- body: %s", rec.Code, rec.Body.String())
	}
	if fake.calls != 1 {
		t.Fatalf("Prometheus was queried %d time(s), want 1", fake.calls)
	}
	// The query reaches Prometheus untouched: with no placeholder to fill, the
	// empty identity must not leak into it as an empty label matcher.
	if strings.Contains(fake.lastQ, `""`) && strings.Contains(fake.lastQ, "namespace=") {
		t.Errorf("query = %s, want no namespace matcher at all", fake.lastQ)
	}
}

// A query about one namespace needs the namespace and nothing else. This is the
// half-way scope, and the one that proves the rule is per-placeholder rather
// than "all or nothing".
func TestNamespaceQueryNeedsOnlyTheNamespace(t *testing.T) {
	fake := newFakePrometheus(oneSeries)
	defer fake.close()

	rec := askMetrics(t, newPrometheusClient(fake.server.URL), map[string]interface{}{
		"metric":      "cpu",
		"namespace":   "gamestest",
		"start":       1785975600,
		"end":         1785979200,
		"granularity": "minute",
		"promql":      `sum(rate(container_cpu_usage_seconds_total{namespace="{namespace}"}[5m]))`,
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 -- body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(fake.lastQ, `namespace="gamestest"`) {
		t.Errorf("query = %s, want the namespace substituted", fake.lastQ)
	}
}

// The relaxation must not become a hole: a query that DOES ask for a placeholder
// still fails without it. Otherwise `{namespace}` would survive into PromQL as
// literal text and Prometheus would answer about a namespace called
// "{namespace}" -- an empty chart with no error anywhere.
func TestPlaceholderWithoutItsValueIsStillRefused(t *testing.T) {
	fake := newFakePrometheus(oneSeries)
	defer fake.close()

	body := validBody()
	delete(body, "namespace")
	rec := askMetrics(t, newPrometheusClient(fake.server.URL), body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "namespace") {
		t.Errorf("body = %q, want the name of the field that was missing", rec.Body.String())
	}
	if fake.calls != 0 {
		t.Errorf("Prometheus was queried %d time(s) for an unsatisfiable request", fake.calls)
	}
}

// The scope survives as words in the log. A cluster-wide failure used to print
// as "cpu / ()", which reads like corrupted input rather than the widest scope
// there is -- and this line is what someone reads to decide whether the fault is
// in one workload or everywhere.
func TestScopeIsReadableAtEveryWidth(t *testing.T) {
	casos := []struct {
		nome string
		body metricsQueryBody
		want string
	}{
		{"workload", metricsQueryBody{Kind: "Deployment", Name: "kuma", Namespace: "gamestest"}, "Deployment/kuma in gamestest"},
		{"namespace", metricsQueryBody{Namespace: "gamestest"}, "namespace gamestest"},
		{"cluster", metricsQueryBody{}, "whole cluster"},
	}
	for _, c := range casos {
		if got := describeScope(c.body); got != c.want {
			t.Errorf("%s: describeScope = %q, want %q", c.nome, got, c.want)
		}
	}
}

// A month at one-minute steps is 44 640 points; Prometheus refuses anything over
// 11 000 with an error the reader cannot act on. Coarsening is the same chart
// that width would show anyway.
func TestLongWindowIsCoarsenedInsteadOfRefused(t *testing.T) {
	fake := newFakePrometheus(oneSeries)
	defer fake.close()

	body := validBody()
	body["start"] = 1785975600
	body["end"] = 1785975600 + 30*24*3600 // 30 days
	rec := askMetrics(t, newPrometheusClient(fake.server.URL), body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if fake.lastStep == "60" {
		t.Errorf("step stayed at 60s over 30 days: that is %d points", 30*24*60)
	}
	var got metricsQueryResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	// The answer has to report the step it really used, or the axis is drawn at
	// the spacing that was asked for rather than the one that was served.
	if got.BucketSeconds != 864 {
		t.Errorf("bucketSeconds = %d, want 864 (30 days / 3000 points)", got.BucketSeconds)
	}
}

// NaN and +Inf are legal Prometheus values and are not JSON numbers. Passing
// them through produces a body the caller cannot parse at all -- one bad point
// would lose the whole series.
func TestNotPlottableValuesBecomeGaps(t *testing.T) {
	fake := newFakePrometheus(`{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{},"values":[[1,"1.5"],[2,"NaN"],[3,"+Inf"],[4,"2.5"]]}
	]}}`)
	defer fake.close()

	rec := askMetrics(t, newPrometheusClient(fake.server.URL), validBody())
	var got metricsQueryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unreadable answer: %v", err)
	}
	if len(got.Points) != 2 {
		t.Fatalf("got %d points, want 2 (NaN and +Inf dropped)", len(got.Points))
	}
	if got.Points[0].Value != 1.5 || got.Points[1].Value != 2.5 {
		t.Errorf("points = %+v, want the two real values", got.Points)
	}
}

// Prometheus explains a bad query in the body; only the status code reaching the
// reader would throw the diagnosis away.
func TestPrometheusErrorTextReachesTheCaller(t *testing.T) {
	fake := newFakePrometheus(`{"status":"error","errorType":"bad_data","error":"parse error: unexpected end of input"}`)
	fake.status = http.StatusBadRequest
	defer fake.close()

	rec := askMetrics(t, newPrometheusClient(fake.server.URL), validBody())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "parse error") {
		t.Errorf("body = %s, want the reason Prometheus gave", rec.Body.String())
	}
}
