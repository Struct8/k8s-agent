package main

import (
	"sync"
	"time"
)

// Resolution of the local store. Five minutes is what makes the window fit the
// Pod's memory: 48 h is 576 points per series, and 2,000 series come to roughly
// 18 MB of samples. At collection resolution (20 s) the same window would pass
// 130 MB and blow the container's memory limit.
//
// It is also the FLOOR for a query: asking for something finer returns
// five-minute buckets, and the response states which bucket was used (see
// query.go). Never hand back five-minute data labelled as one-minute data.
const bucketSeconds int64 = 300

type seriesKey struct {
	Metric    string
	Namespace string
	Kind      string
	Name      string
}

// One closed interval. It keeps sum and count rather than a pre-computed mean:
// the mean of a wider interval has to be the mean of the SAMPLES, not the mean
// of the means -- with uneven counts per bucket those two diverge.
type storedBucket struct {
	start int64
	sum   float64
	count int
}

type storedSeries struct {
	buckets   []storedBucket
	lastWrite int64
}

// metricStore is a circular buffer over TIME: a bucket's index comes from the
// instant itself (`start / bucketSeconds % capacity`), and every bucket carries
// the `start` it represents.
//
// That removes any need for an expiry sweep. When time wraps, the slot is reused
// and the recorded `start` stops matching what a query asks for, so stale data
// is simply never read. A bucket outside the window is indistinguishable from
// one that never existed, which is exactly what is wanted.
type metricStore struct {
	mu        sync.Mutex
	series    map[seriesKey]*storedSeries
	capacity  int
	maxSeries int
}

func newMetricStore(retentionHours, maxSeries int) *metricStore {
	capacity := retentionHours * 3600 / int(bucketSeconds)
	if capacity < 1 {
		capacity = 1
	}
	if maxSeries < 1 {
		maxSeries = 1
	}
	return &metricStore{
		series:    make(map[seriesKey]*storedSeries),
		capacity:  capacity,
		maxSeries: maxSeries,
	}
}

// RetentionSeconds is the real window the buffer covers.
func (s *metricStore) RetentionSeconds() int64 {
	return int64(s.capacity) * bucketSeconds
}

func (s *metricStore) SeriesCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.series)
}

// Append records a whole collection cycle into the bucket holding `now`.
//
// The ~15 samples landing in one bucket (collection every 20 s) ACCUMULATE: the
// last one does not overwrite the earlier ones. Overwriting would turn the chart
// into an instantaneous reading every five minutes, which is a different metric
// -- and one that hides spikes.
func (s *metricStore) Append(points []metricPoint, now time.Time) {
	if len(points) == 0 {
		return
	}
	nowUnix := now.Unix()
	start := nowUnix - nowUnix%bucketSeconds

	s.mu.Lock()
	defer s.mu.Unlock()

	idx := int((start / bucketSeconds) % int64(s.capacity))
	for _, p := range points {
		key := seriesKey{Metric: p.Metric, Namespace: p.Namespace, Kind: p.Kind, Name: p.Name}
		serie := s.series[key]
		if serie == nil {
			if len(s.series) >= s.maxSeries {
				s.evictOldestLocked()
			}
			serie = &storedSeries{buckets: make([]storedBucket, s.capacity)}
			s.series[key] = serie
		}

		b := &serie.buckets[idx]
		if b.start != start {
			// Slot from another turn of the clock (or never used): start over.
			b.start, b.sum, b.count = start, 0, 0
		}
		b.sum += p.Value
		b.count++
		serie.lastWrite = nowUnix
	}
}

// evictOldestLocked drops the least recently written series. It runs only when
// the map is at its ceiling, so the O(n) scan happens when a new series is
// inserted, not on every point.
func (s *metricStore) evictOldestLocked() {
	var target seriesKey
	var oldest int64
	first := true
	for k, serie := range s.series {
		if first || serie.lastWrite < oldest {
			target, oldest, first = k, serie.lastWrite, false
		}
	}
	if !first {
		delete(s.series, target)
	}
}

type queryPoint struct {
	Bucket int64   `json:"bucket"`
	Value  float64 `json:"value"`
}

// Query returns the series aggregated into `step`-second steps, plus the bucket
// size actually used.
//
// `step` is raised to the floor of `bucketSeconds` and aligned to a multiple of
// it -- asking for 60 s returns 300 s, and the response says so instead of
// pretending otherwise.
func (s *metricStore) Query(key seriesKey, from, to, step int64) (int64, []queryPoint) {
	if step < bucketSeconds {
		step = bucketSeconds
	}
	step -= step % bucketSeconds

	if to <= from {
		return step, nil
	}
	// Never scan beyond what the buffer covers: a request spanning years would
	// become millions of iterations to return the same few live buckets.
	if limit := to - s.RetentionSeconds(); from < limit {
		from = limit
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	serie := s.series[key]
	if serie == nil {
		return step, nil
	}

	var out []queryPoint
	var window int64 = -1
	var sum float64
	var count int

	flush := func() {
		if count > 0 {
			out = append(out, queryPoint{Bucket: window, Value: sum / float64(count)})
		}
		sum, count = 0, 0
	}

	begin := from - from%bucketSeconds
	for t := begin; t < to; t += bucketSeconds {
		current := t - t%step
		if current != window {
			flush()
			window = current
		}
		b := serie.buckets[int((t/bucketSeconds)%int64(s.capacity))]
		if b.start != t || b.count == 0 {
			continue // slot from another turn, or an interval with no collection
		}
		sum += b.sum
		count += b.count
	}
	flush()

	return step, out
}
