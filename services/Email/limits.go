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

// InvitationReserve is how many of the day's capped messages an invitation
// leaves unspent (SendOptions.Keep): password resets share the cap, and a
// person locked out should still get theirs after an admin has invited a whole
// imported team. At most a quarter of the cap is kept (kept).
const InvitationReserve = 5

// kept is how many of a day's cap a send asking to keep keep holds back: at
// most a quarter of the cap. With the whole reserve, a cap of 5 or less
// (EMAIL_DAILY_CAP) left no invitation to email at all. Pure.
func kept(cap, keep int) int {
	if quarter := cap / 4; keep > quarter {
		return quarter
	}
	return keep
}

// dailyCounter counts messages sent in the current UTC day.
type dailyCounter struct {
	mu    sync.Mutex
	day   string
	count int
}

var sentToday dailyCounter

// take reserves one message against the cap, or refuses it. keep is how many
// of the day's messages must be left after this one (an invitation keeps
// InvitationReserve). Pure given its arguments and the counter.
func (c *dailyCounter) take(now time.Time, cap, keep int) error {
	if cap <= 0 {
		return nil
	}
	keep = kept(cap, keep)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.roll(now)
	if c.count >= cap {
		msg := "this workspace has sent its " + strconv.Itoa(cap) + " emails for today; it can send more tomorrow (UTC)"
		return &SendError{
			StatusCode: http.StatusTooManyRequests,
			Message:    msg,
			Terminal:   false,
			ForPeople:  msg,
		}
	}
	if c.count >= cap-keep {
		msg := "today's emails for invitations are used up (the last " + strconv.Itoa(keep) + " of the day's " + strconv.Itoa(cap) +
			" are kept for password resets); it can send more tomorrow (UTC)"
		return &SendError{
			StatusCode: http.StatusTooManyRequests,
			Message:    msg,
			Terminal:   false,
			ForPeople:  msg,
		}
	}
	c.count++
	return nil
}

// left is how many more messages the day has after keeping keep. Pure given
// its arguments and the counter.
func (c *dailyCounter) left(now time.Time, cap, keep int) int {
	keep = kept(cap, keep)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.roll(now)
	if n := cap - keep - c.count; n > 0 {
		return n
	}
	return 0
}

// roll starts a new count on a new UTC day. Called with c.mu held.
func (c *dailyCounter) roll(now time.Time) {
	if day := now.UTC().Format("2006-01-02"); c.day != day {
		c.day, c.count = day, 0
	}
}

// InvitationsLeftToday is how many more invitations can be emailed today
// before the reserve for password resets, and whether the day has a cap at
// all (when it doesn't, left means nothing). Email being off is
// IsEmailEnabled's to say.
func InvitationsLeftToday() (left int, capped bool) {
	cap := dailyCapFromEnv()
	if cap <= 0 {
		return 0, false
	}
	return sentToday.left(time.Now(), cap, InvitationReserve), true
}
