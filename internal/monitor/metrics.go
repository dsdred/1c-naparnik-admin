package monitor

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"naparnikadmin/internal/gateway"
)

// Metrics хранит счётчики одного экземпляра приложения.
type Metrics struct {
	http.Handler
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	tokens   *prometheus.CounterVec
}

// NewMetrics создаёт изолированный реестр для слушателя Prometheus.
func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()
	up := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "naparnik_admin_up",
		Help: "Whether the Naparnik Admin metrics listener is serving.",
	})
	up.Set(1)
	m := &Metrics{
		Handler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "naparnik_admin_gateway_requests_total",
			Help: "Completed gateway engine requests by protocol and logical status code.",
		}, []string{"protocol", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "naparnik_admin_gateway_request_duration_seconds",
			Help:    "Gateway engine request duration, including session queue time.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}, []string{"protocol"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "naparnik_admin_gateway_tokens_total",
			Help: "Tokens reported by Naparnik. Cached tokens are a subset of input tokens.",
		}, []string{"protocol", "kind"}),
	}
	registry.MustRegister(up, m.requests, m.duration, m.tokens)
	for _, protocol := range []string{"openai", "responses", "anthropic", "gemini", "unknown"} {
		m.requests.WithLabelValues(protocol, "200")
		m.duration.WithLabelValues(protocol)
		for _, kind := range []string{"input", "output", "cached"} {
			m.tokens.WithLabelValues(protocol, kind)
		}
	}
	return m
}

// ObserveRequest учитывает результат выполнения, даже если SSE уже отправил HTTP 200.
func (m *Metrics) ObserveRequest(protocol string, duration time.Duration, reply *gateway.Reply, err error) {
	switch protocol {
	case "openai", "responses", "anthropic", "gemini":
	default:
		protocol = "unknown"
	}
	code := http.StatusOK
	if err != nil {
		code = gateway.FromError(err).Status
		if code < 400 || code > 599 {
			code = http.StatusInternalServerError
		}
	}
	m.requests.WithLabelValues(protocol, strconv.Itoa(code)).Inc()
	m.duration.WithLabelValues(protocol).Observe(duration.Seconds())
	if reply != nil && reply.Usage != nil {
		u := reply.Usage
		m.tokens.WithLabelValues(protocol, "input").Add(float64(max(u.PromptTokens, 0)))
		m.tokens.WithLabelValues(protocol, "output").Add(float64(max(u.CompletionTokens, 0)))
		m.tokens.WithLabelValues(protocol, "cached").Add(float64(max(u.CachedTokens, 0)))
	}
}
