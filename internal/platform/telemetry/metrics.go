package telemetry

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	Registry *prometheus.Registry

	RunRequests       *prometheus.CounterVec
	RunDuration       *prometheus.HistogramVec
	RunVerdicts       *prometheus.CounterVec
	StepQueueWait     *prometheus.HistogramVec
	StepDuration      *prometheus.HistogramVec
	StepRetries       *prometheus.CounterVec
	SchedulerBacklog  *prometheus.GaugeVec
	WorkerUtilization *prometheus.GaugeVec
	CASTransferBytes  *prometheus.CounterVec
	CacheHits         *prometheus.CounterVec
	Counterexamples   *prometheus.CounterVec
	EvidenceInvalid   *prometheus.CounterVec
	PolicyBlock       *prometheus.CounterVec
	OutboxPublished   *prometheus.CounterVec
	OutboxFailures    *prometheus.CounterVec
	LeaseExpired      *prometheus.CounterVec
	ActiveRuns        *prometheus.GaugeVec
	ActiveSteps       *prometheus.GaugeVec
	HTTPRequests      *prometheus.CounterVec
	HTTPDuration      *prometheus.HistogramVec
	EngineErrors      *prometheus.CounterVec

	inflightSteps *prometheus.GaugeVec
}

func NewMetrics() *Metrics {
	registry := prometheus.NewRegistry()
	m := &Metrics{Registry: registry}
	m.RunRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_run_requests_total", Help: "Verification runs accepted by the API.",
	}, []string{"region", "client_type"})
	m.RunDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "patchproof_run_duration_seconds", Help: "End-to-end run duration.", Buckets: []float64{30, 60, 120, 300, 600, 900, 1800, 3600, 7200},
	}, []string{"region", "project_tier", "verdict"})
	m.RunVerdicts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_run_verdicts_total", Help: "Final run verdicts.",
	}, []string{"region", "verdict"})
	m.StepQueueWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "patchproof_step_queue_wait_seconds", Help: "Time between step readiness and lease acquisition.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600},
	}, []string{"region", "step_kind", "pool"})
	m.StepDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "patchproof_step_duration_seconds", Help: "Step execution duration.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200},
	}, []string{"engine", "step_kind", "pool"})
	m.StepRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_step_retries_total", Help: "Step retry decisions.",
	}, []string{"error_class", "region"})
	m.SchedulerBacklog = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "patchproof_scheduler_backlog", Help: "Ready steps not yet leased.",
	}, []string{"region", "priority", "pool"})
	m.WorkerUtilization = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "patchproof_worker_utilization", Help: "Observed cluster utilisation.",
	}, []string{"cluster", "pool", "resource"})
	m.CASTransferBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_cas_transfer_bytes_total", Help: "Artifact bytes transferred.",
	}, []string{"direction", "region"})
	m.CacheHits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_cache_hit_total", Help: "Action cache lookups served from cache.",
	}, []string{"cache_kind", "engine"})
	m.Counterexamples = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_counterexamples_total", Help: "Reproduced counterexamples.",
	}, []string{"claim_kind", "language"})
	m.EvidenceInvalid = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_evidence_invalid_total", Help: "Evidence rejected during integrity checks.",
	}, []string{"reason", "region"})
	m.PolicyBlock = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_policy_block_total", Help: "Runs blocked by trusted policy.",
	}, []string{"policy_class"})
	m.OutboxPublished = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_outbox_published_total", Help: "Outbox events published.",
	}, []string{"event_type"})
	m.OutboxFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_outbox_failures_total", Help: "Outbox delivery failures.",
	}, []string{"event_type"})
	m.LeaseExpired = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_lease_expired_total", Help: "Expired step leases reclaimed.",
	}, []string{"region"})
	m.ActiveRuns = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "patchproof_active_runs", Help: "Runs in non-terminal states.",
	}, []string{"region", "status"})
	m.ActiveSteps = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "patchproof_active_steps", Help: "Steps currently leased or running.",
	}, []string{"region", "step_kind"})
	m.HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_http_requests_total", Help: "HTTP requests handled by the API.",
	}, []string{"method", "route", "status"})
	m.HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "patchproof_http_request_duration_seconds", Help: "HTTP request latency.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route"})
	m.EngineErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "patchproof_engine_errors_total", Help: "Engine failures by classification.",
	}, []string{"engine", "error_class"})
	m.inflightSteps = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "patchproof_inflight_steps", Help: "Steps leased to an executor.",
	}, []string{"cluster"})

	registry.MustRegister(
		m.RunRequests, m.RunDuration, m.RunVerdicts, m.StepQueueWait, m.StepDuration,
		m.StepRetries, m.SchedulerBacklog, m.WorkerUtilization, m.CASTransferBytes,
		m.CacheHits, m.Counterexamples, m.EvidenceInvalid, m.PolicyBlock, m.OutboxPublished,
		m.OutboxFailures, m.LeaseExpired, m.ActiveRuns, m.ActiveSteps, m.HTTPRequests,
		m.HTTPDuration, m.EngineErrors, m.inflightSteps,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

func (m *Metrics) ObserveHTTP(method, route string, status int, duration time.Duration) {
	m.HTTPRequests.WithLabelValues(method, route, statusClass(status)).Inc()
	m.HTTPDuration.WithLabelValues(method, route).Observe(duration.Seconds())
}

func (m *Metrics) IncInflight(cluster string) {
	m.inflightSteps.WithLabelValues(cluster).Inc()
}

func (m *Metrics) DecInflight(cluster string) {
	m.inflightSteps.WithLabelValues(cluster).Dec()
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}
