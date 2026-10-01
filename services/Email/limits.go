package email

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limits for a workspace whose email is lent to it by OneCamp Cloud.
//
// A managed workspace on a onemana.dev address sends through OneCamp's own
// email account, which is a free tier shared by every workspace and by the
// store itself: 100 messages a day in all. Two settings keep one busy team
// from spending everybody's day:
//
//   - EMAIL_ESSENTIAL_ONLY=true sends only what someone is waiting on, an
//     invitation or a password reset, and no notification email at all. In-app
//     and push notifications are unaffected.
//   - EMAIL_DAILY_CAP=n stops sending after n messages in a UTC day. What is
//     refused is refused with a retryable error, so a caller that retries does
//     so tomorrow rather than never.
//
// A self-hosted workspace with its own sending key sets neither and behaves as
// it always has.

// EssentialOnly reports whether only invitations and password resets are sent.
func EssentialOnly() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("EMAIL_ESSENTIAL_ONLY")))
	return v == "true" || v == "1" || v == "yes"
}

// NotificationEmailEnabled is whether notification email (mentions, digests,
// reminders) goes out: email is on and not restricted to the essentials.
func NotificationEmailEnabled() bool {
	return IsEmailEnabled() && !EssentialOnly()
}

// dailyCapFromEnv is EMAIL_DAILY_CAP, or 0 for no cap.
func dailyCapFromEnv() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("EMAIL_DAILY_CAP")))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// dailyCounter counts messages sent in the current UTC day.
type dailyCounter struct {
	mu    sync.Mutex
	day   string
	count int
}

var sentToday dailyCounter

// take reserves one message against the cap, or refuses it. Pure given its
// arguments and the counter.
func (c *dailyCounter) take(now time.Time, cap int) error {
	if cap <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	day := now.UTC().Format("2006-01-02")
	if c.day != day {
		c.day, c.count = day, 0
	}
	if c.count >= cap {
		return &SendError{
			StatusCode: http.StatusTooManyRequests,
			Message:    "this workspace has sent its " + strconv.Itoa(cap) + " emails for today; it can send more tomorrow (UTC)",
			Terminal:   false,
		}
	}
	c.count++
	return nil
}
