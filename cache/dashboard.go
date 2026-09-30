package cache

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

//go:embed dashboard.html
var dashboardHTML []byte

// Dashboard serves the metrics, on its own port.
//
// Separate from the proxy so that scraping and a browser polling every couple of seconds
// never share a handler with the hot path — and so the proxy port stays a pure Ollama
// endpoint, with nothing on it that a client pointed here could trip over.
type Dashboard struct {
	cache   *Cache
	metrics *Metrics
}

func NewDashboard(cache *Cache, metrics *Metrics) *Dashboard {
	return &Dashboard{cache: cache, metrics: metrics}
}

func (dashboard *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", dashboard.handleMetrics)
	mux.HandleFunc("/api/stats", dashboard.handleStats)
	mux.HandleFunc("/", dashboard.handleIndex)
	return mux
}

func (dashboard *Dashboard) handleMetrics(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	dashboard.metrics.WritePrometheus(writer, dashboard.cache)
}

func (dashboard *Dashboard) handleStats(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(dashboard.metrics.Snapshot(dashboard.cache)) //nolint:errcheck
}

func (dashboard *Dashboard) handleIndex(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Write(dashboardHTML) //nolint:errcheck
}
