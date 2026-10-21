package business

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The outgoing deliveries' counters, under the names business/Webhook gave
// them when it sent from memory, so a dashboard reading them goes on working.
// A dispatch is one try now; a retry is a try scheduled after a failure; a
// failure is a delivery that failed every try.
var (
	tries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "webhook_dispatches_total",
		Help: "Outgoing webhook tries by event type and outcome.",
	}, []string{"event_type", "status"})

	tryDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "webhook_dispatch_duration_seconds",
		Help:    "Outgoing webhook try latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"event_type"})

	retries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "webhook_retries_total",
		Help: "Outgoing webhook tries scheduled again after a failure, by event type.",
	}, []string{"event_type"})

	gaveUp = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "webhook_failures_total",
		Help: "Outgoing webhook deliveries that failed every try, by event type.",
	}, []string{"event_type"})
)
