package business

import (
	"context"
	"time"
)

// stubSession is the demo's mailbox (helpers.DemoMode): it never dials and
// holds three canned messages, a member's (a demo cast address), an
// outsider's and a repeat of the first one's Message-ID, so a walk sees mail
// arrive in triage, a sender named as the member, and the repeat dropped. It
// takes no credentials and keeps none.
type stubSession struct{}

func openStub(context.Context, Box) (Session, error) { return &stubSession{}, nil }

// stubValidity is the stub's UIDVALIDITY; its UIDs are 1 to 3.
const stubValidity = 1

func (*stubSession) Validity() uint32 { return stubValidity }

// Next is 1: a stub mailbox delivers its messages on its first read after
// connecting, unlike a real one, which starts from what arrives next.
func (*stubSession) Next() uint32 { return 1 }
func (*stubSession) Close()       {}

func (s *stubSession) After(_ context.Context, uid uint32, max int) ([]Fetched, bool, error) {
	var hits []Fetched
	for _, m := range stubMessages() {
		if m.UID > uid {
			hits = append(hits, m)
		}
	}
	out, more := firstWithin(hits, max)
	return out, more, nil
}

func (s *stubSession) Since(ctx context.Context, _ time.Time, max int) ([]Fetched, bool, error) {
	return s.After(ctx, 0, max)
}

func stubMessages() []Fetched {
	msg := func(uid uint32, id, from, subject, body string) Fetched {
		raw := "Message-ID: " + id + "\r\nFrom: " + from + "\r\nSubject: " + subject +
			"\r\nDate: Tue, 17 Nov 2026 09:00:00 +0000\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body
		return Fetched{UID: uid, Size: int64(len(raw)), Raw: []byte(raw)}
	}
	return []Fetched{
		msg(1, "<demo-1@mail.demo.invalid>", "Maya Chen <maya.chen@cast.demo.onemana.dev>", "The export button does nothing on Safari",
			"Clicking Export on the reports page does nothing in Safari 18. Chrome works."),
		msg(2, "<demo-2@mail.demo.invalid>", "Priya from Acme <priya@acme.example>", "Can we get invoices as PDF?",
			"Hi! Our finance team needs each month's invoice as a PDF. Is that possible?"),
		msg(3, "<demo-1@mail.demo.invalid>", "Maya Chen <maya.chen@cast.demo.onemana.dev>", "The export button does nothing on Safari",
			"(the same message again: the door takes it once)"),
	}
}
