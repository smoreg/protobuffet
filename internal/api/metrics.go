package api

import (
	"expvar"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Metrics are exported in the Prometheus text format from /metrics. The
// registry has no metrics dependency: the handful of counters a schema
// registry needs is not worth a client library, and expvar-style counters
// render into the text format in a few lines.
type metrics struct {
	mu sync.Mutex

	requests  map[requestKey]int64
	durations map[string]*histogram
	publishes map[string]int64 // result -> count
}

type requestKey struct {
	route  string
	status int
}

type histogram struct {
	count   int64
	sum     float64
	buckets [7]int64 // .005 .025 .1 .5 1 5 +Inf
}

var histogramBounds = [6]float64{0.005, 0.025, 0.1, 0.5, 1, 5}

func newMetrics() *metrics {
	return &metrics{
		requests:  map[requestKey]int64{},
		durations: map[string]*histogram{},
		publishes: map[string]int64{},
	}
}

func (m *metrics) observe(route string, status int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.requests[requestKey{route: route, status: status}]++
	h, ok := m.durations[route]
	if !ok {
		h = &histogram{}
		m.durations[route] = h
	}
	secs := d.Seconds()
	h.count++
	h.sum += secs
	for i, bound := range histogramBounds {
		if secs <= bound {
			h.buckets[i]++
		}
	}
	h.buckets[len(h.buckets)-1]++
}

func (m *metrics) publish(result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishes[result]++
}

// writeTo renders the Prometheus exposition format.
func (m *metrics) writeTo(w http.ResponseWriter, modules, versions int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Fprint(w, "# HELP protobuffet_http_requests_total Requests by route and status.\n") //nolint:errcheck
	fmt.Fprint(w, "# TYPE protobuffet_http_requests_total counter\n")                       //nolint:errcheck
	for k, v := range m.requests {
		fmt.Fprintf(w, "protobuffet_http_requests_total{route=%q,status=\"%d\"} %d\n", k.route, k.status, v) //nolint:errcheck
	}

	fmt.Fprint(w, "# HELP protobuffet_http_request_duration_seconds Request duration by route.\n") //nolint:errcheck
	fmt.Fprint(w, "# TYPE protobuffet_http_request_duration_seconds histogram\n")                  //nolint:errcheck
	for route, h := range m.durations {
		for i, bound := range histogramBounds {
			fmt.Fprintf(w, "protobuffet_http_request_duration_seconds_bucket{route=%q,le=\"%g\"} %d\n", route, bound, h.buckets[i]) //nolint:errcheck
		}
		fmt.Fprintf(w, "protobuffet_http_request_duration_seconds_bucket{route=%q,le=\"+Inf\"} %d\n", route, h.count) //nolint:errcheck
		fmt.Fprintf(w, "protobuffet_http_request_duration_seconds_sum{route=%q} %g\n", route, h.sum)                  //nolint:errcheck
		fmt.Fprintf(w, "protobuffet_http_request_duration_seconds_count{route=%q} %d\n", route, h.count)              //nolint:errcheck
	}

	fmt.Fprint(w, "# HELP protobuffet_publishes_total Publish attempts by outcome.\n") //nolint:errcheck
	fmt.Fprint(w, "# TYPE protobuffet_publishes_total counter\n")                      //nolint:errcheck
	for result, v := range m.publishes {
		fmt.Fprintf(w, "protobuffet_publishes_total{result=%q} %d\n", result, v) //nolint:errcheck
	}

	fmt.Fprint(w, "# HELP protobuffet_modules Modules in the registry.\n") //nolint:errcheck
	fmt.Fprint(w, "# TYPE protobuffet_modules gauge\n")                    //nolint:errcheck
	fmt.Fprintf(w, "protobuffet_modules %d\n", modules)                    //nolint:errcheck
	fmt.Fprint(w, "# HELP protobuffet_versions Published versions.\n")     //nolint:errcheck
	fmt.Fprint(w, "# TYPE protobuffet_versions gauge\n")                   //nolint:errcheck
	fmt.Fprintf(w, "protobuffet_versions %d\n", versions)                  //nolint:errcheck
}

// route reduces a URL path to a bounded label. Module names and versions must
// never become label values: a registry with a thousand modules would
// otherwise produce a thousand time series per status code.
func route(path string) string {
	switch {
	case path == "/healthz", path == "/metrics", path == "/":
		return path
	case hasPrefix(path, "/go/"):
		return "/go"
	case hasPrefix(path, "/composer/"):
		return "/composer"
	case hasPrefix(path, "/npm/"):
		return "/npm"
	case hasPrefix(path, "/pypi/"):
		return "/pypi"
	case hasPrefix(path, "/api/v1/modules"):
		return "/api/v1/modules"
	case hasPrefix(path, "/m/"):
		return "/m"
	case hasPrefix(path, "/gen/"):
		return "/gen"
	default:
		return "other"
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// expvar is imported so a future debug endpoint can reuse it without another
// dependency; referencing it here keeps the import honest.
var _ = expvar.Get
