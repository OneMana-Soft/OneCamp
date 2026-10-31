package helpers

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRetryAfter is the longest wait RetryAfter gives: a server asking for
// days would otherwise park an import or a retry loop for that long.
const maxRetryAfter = time.Hour

// RetryAfter is how long a server's Retry-After asks the caller to wait:
// seconds, or an HTTP date still to come, at most maxRetryAfter. Without one
// it can use (none, zero, negative, unreadable or past) it is fallback.
func RetryAfter(h http.Header, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return fallback
	}
	var d time.Duration
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return fallback
		}
		if secs > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		d = time.Duration(secs) * time.Second
	} else if at, err := http.ParseTime(v); err == nil {
		d = time.Until(at)
	}
	if d <= 0 {
		return fallback
	}
	return min(d, maxRetryAfter)
}
