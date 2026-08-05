package main

import (
	"testing"
	"time"
)

// The store is what the chart reads. A mistake here does not raise: it returns
// HTTP 200 with a plausible, wrong series -- the worst shape a metric defect can
// take, because nobody questions a chart that rendered. Each case therefore
// asserts the expected VALUE, not merely that something came back.

func samplePoint(name string, value float64) metricPoint {
	return metricPoint{
		Metric:    "cpu_cores",
		Namespace: "default",
		Kind:      "Deployment",
		Name:      name,
		Value:     value,
	}
}

func sampleKey(name string) seriesKey {
	return seriesKey{Metric: "cpu_cores", Namespace: "default", Kind: "Deployment", Name: name}
}

// An instant aligned to the start of a bucket, so the cases do not depend on
// where the real clock happened to land.
func at(unix int64) time.Time { return time.Unix(unix, 0) }

func TestAppendAveragesSamplesInTheSameBucket(t *testing.T) {
	s := newMetricStore(48, 100)
	base := int64(1_000_000_000)
	base -= base % bucketSeconds

	// Three collections inside the SAME five-minute bucket (20 s apart).
	s.Append([]metricPoint{samplePoint("api", 1)}, at(base))
	s.Append([]metricPoint{samplePoint("api", 2)}, at(base+20))
	s.Append([]metricPoint{samplePoint("api", 6)}, at(base+40))

	bucket, points := s.Query(sampleKey("api"), base, base+bucketSeconds, 60)
	if bucket != bucketSeconds {
		t.Fatalf("bucket returned = %d, expected %d", bucket, bucketSeconds)
	}
	if len(points) != 1 {
		t.Fatalf("expected 1 point, got %d: %+v", len(points), points)
	}
	// Mean (1+2+6)/3 = 3. Overwriting would give 6 -- and a chart built from
	// instantaneous readings every five minutes hides everything in between.
	if points[0].Value != 3 {
		t.Fatalf("value = %v, expected 3 (mean of the samples, not the last one)", points[0].Value)
	}
}

func TestQueryRaisesGranularityToTheFloor(t *testing.T) {
	s := newMetricStore(48, 100)
	base := int64(1_000_000_000)
	base -= base % bucketSeconds
	s.Append([]metricPoint{samplePoint("api", 5)}, at(base))

	// Callers ask for `minute` on short windows. The store has no such
	// resolution, and the answer must SAY so rather than label five-minute data
	// as one-minute -- otherwise the chart axis comes out wrongly spaced.
	bucket, _ := s.Query(sampleKey("api"), base, base+bucketSeconds, 60)
	if bucket != 300 {
		t.Fatalf("60s granularity should become 300s, got %d", bucket)
	}
}

func TestQueryAggregatesBySampleNotByBucketMean(t *testing.T) {
	s := newMetricStore(48, 100)
	// Aligned to the STEP (600), not to the bucket. Aggregation windows are
	// absolute -- counted from the epoch -- so two neighbouring buckets only
	// fall in the same ten-minute window when the start is aligned to ten
	// minutes.
	base := int64(1_000_000_000)
	base -= base % 600

	// Bucket 1: one sample worth 10. Bucket 2: three samples worth 0.
	s.Append([]metricPoint{samplePoint("api", 10)}, at(base))
	for i := 0; i < 3; i++ {
		s.Append([]metricPoint{samplePoint("api", 0)}, at(base+bucketSeconds+int64(i*20)))
	}

	_, points := s.Query(sampleKey("api"), base, base+2*bucketSeconds, 600)
	if len(points) != 1 {
		t.Fatalf("expected 1 aggregated point, got %d: %+v", len(points), points)
	}
	// Mean of the SAMPLES: (10+0+0+0)/4 = 2.5. Mean of the MEANS would give
	// (10+0)/2 = 5 -- double, and the error grows as the counts get more uneven.
	if points[0].Value != 2.5 {
		t.Fatalf("value = %v, expected 2.5 (per-sample mean, not mean of means)", points[0].Value)
	}
}

func TestPointOutsideTheWindowDisappearsWhenTheRingWraps(t *testing.T) {
	// One hour of retention = 12 five-minute buckets.
	s := newMetricStore(1, 100)
	base := int64(1_000_000_000)
	base -= base % bucketSeconds

	s.Append([]metricPoint{samplePoint("api", 7)}, at(base))

	// Exactly one turn later the same slot is reused by another instant. The old
	// value must not reappear in a query about the past.
	oneTurn := base + s.RetentionSeconds()
	s.Append([]metricPoint{samplePoint("api", 9)}, at(oneTurn))

	_, old := s.Query(sampleKey("api"), base, base+bucketSeconds, 300)
	if len(old) != 0 {
		t.Fatalf("a point outside the window should be gone, got %+v", old)
	}

	_, fresh := s.Query(sampleKey("api"), oneTurn, oneTurn+bucketSeconds, 300)
	if len(fresh) != 1 || fresh[0].Value != 9 {
		t.Fatalf("the new point should occupy the reused slot, got %+v", fresh)
	}
}

func TestSeriesCeilingDropsTheLeastRecent(t *testing.T) {
	s := newMetricStore(48, 2)
	base := int64(1_000_000_000)
	base -= base % bucketSeconds

	s.Append([]metricPoint{samplePoint("oldest", 1)}, at(base))
	s.Append([]metricPoint{samplePoint("middle", 1)}, at(base+bucketSeconds))
	// The third series exceeds the ceiling and evicts the least recently written.
	s.Append([]metricPoint{samplePoint("newest", 1)}, at(base+2*bucketSeconds))

	if got := s.SeriesCount(); got != 2 {
		t.Fatalf("series kept = %d, expected 2 (the ceiling)", got)
	}
	if _, points := s.Query(sampleKey("oldest"), base, base+bucketSeconds, 300); len(points) != 0 {
		t.Fatalf("the oldest series should have been evicted, got %+v", points)
	}
	if _, points := s.Query(sampleKey("newest"), base+2*bucketSeconds, base+3*bucketSeconds, 300); len(points) != 1 {
		t.Fatalf("the newest series should be kept, got %+v", points)
	}
}

func TestQueryOfUnknownSeriesDoesNotBreak(t *testing.T) {
	s := newMetricStore(48, 100)
	bucket, points := s.Query(sampleKey("never-seen"), 1000, 2000, 300)
	if bucket != 300 || len(points) != 0 {
		t.Fatalf("an unknown series should return an empty list, got bucket=%d points=%+v", bucket, points)
	}
}

func TestWindowWiderThanRetentionDoesNotScanAllOfHistory(t *testing.T) {
	// One hour of retention, a request spanning ten years. Without the clamp the
	// scan would be millions of iterations to return the same 12 live buckets.
	s := newMetricStore(1, 100)
	base := int64(1_000_000_000)
	base -= base % bucketSeconds
	s.Append([]metricPoint{samplePoint("api", 4)}, at(base))

	started := time.Now()
	_, points := s.Query(sampleKey("api"), base-10*365*24*3600, base+bucketSeconds, 300)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("an absurd window took %v -- the retention clamp is not working", elapsed)
	}
	if len(points) != 1 || points[0].Value != 4 {
		t.Fatalf("the live point should still come back, got %+v", points)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	// Every `false` here is a port published outside the Pod. `:8080` is the most
	// deceptive case: it looks confined and means every interface.
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"localhost:8080": true,
		"[::1]:8080":     true,
		"0.0.0.0:8080":   false,
		":8080":          false,
		"10.0.0.5:8080":  false,
		"no-port":        false,
	}
	for addr, expected := range cases {
		if got := isLoopbackAddr(addr); got != expected {
			t.Errorf("isLoopbackAddr(%q) = %v, expected %v", addr, got, expected)
		}
	}
}
