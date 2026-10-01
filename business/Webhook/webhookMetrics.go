package business

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	webhookDispatchesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "webhook_dispatches_total",
		Help: "Total outgoing webhook dispatches by event type and status.",
	}, []string{"event_type", "status"})

	webhookDispatchDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "webhook_dispatch_duration_seconds",
		Help:    "Outgoing webhook dispatch latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"event_type"})

	webhookRetriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "webhook_retries_total",
		Help: "Total webhook dispatch retries by event type.",
	}, []string{"event_type"})

	webhookFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "webhook_failures_total",
		Help: "Total webhook dispatch failures by event type.",
	}, []string{"event_type"})

	webhookRateLimitedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "webhook_rate_limited_total",
		Help: "Total incoming requests rate-limited by the webhook rate limiter.",
	})
)
