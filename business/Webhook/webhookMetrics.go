package business

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// The outgoing deliveries' counters are business/Hooks's (metrics.go).
	webhookRateLimitedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "webhook_rate_limited_total",
		Help: "Total incoming requests rate-limited by the webhook rate limiter.",
	})
)
