package email

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// "An email is on its way" used to mean only that a key was set; the send's
// error was dropped. The admin is now told what happened, in words: these are
// the words for each way a send fails.
func TestASendThatFailedSaysWhy(t *testing.T) {
	capped := (&dailyCounter{day: time.Now().UTC().Format("2006-01-02"), count: 5}).take(time.Now(), 5, 0)
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{ErrNotConfigured, "email isn't set up on this server"},
		{fmt.Errorf("%w %q: x", ErrBadSender, "noreply@"), "the sender address set in Admin > Email isn't a valid address"},
		{fmt.Errorf("%w %q: x", ErrBadRecipient, "ana@"), "that isn't a valid email address"},
		{ErrUndeliverable, "that address's domain can't receive email"},
		{capped, "this workspace has sent its 5 emails for today; it can send more tomorrow (UTC)"},
		{fmt.Errorf("send: %w", context.DeadlineExceeded), "the email provider didn't answer in time"},
		{&SendError{StatusCode: 403, Detail: "The onemana.dev domain is not verified."},
			"the email provider refused it (The onemana.dev domain is not verified)"},
		{&SendError{StatusCode: 422}, "the email provider refused it (status 422)"},
		{&SendError{StatusCode: 429, Detail: "Too many requests"}, "the email provider is sending too much at once; try again in a minute"},
		{&SendError{StatusCode: 503}, "the email provider had a problem; try again later"},
		{errors.New("dial tcp: no such host"), "the email provider couldn't be reached"},
	}
	for _, c := range cases {
		if got := Reason(c.err); got != c.want {
			t.Errorf("Reason(%v) = %q, want %q", c.err, got, c.want)
		}
		if c.err != nil && strings.Contains(Reason(c.err), "RESEND_API_KEY") {
			t.Errorf("Reason(%v) names an environment variable to an admin", c.err)
		}
	}
}

// A send with no key says so before anything else, rather than failing on
// the addresses or the network.
func TestSendingWithoutAKeyIsRefusedPlainly(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	_, err := SendEmailWithOptions(context.Background(), SendOptions{From: "a@b.test", To: "c@d.test", Subject: "s", HTML: "<p>h</p>"})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}
