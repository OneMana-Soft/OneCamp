package business

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"github.com/google/uuid"
)

// gmailClient.go — thin, per-user Gmail operations. Every function resolves a
// token strictly from the passed userUUID (never a shared/service token), so a
// user's mail is only ever accessed on their own behalf.

// gmailService builds an authenticated Gmail client for a user, or returns
// (nil, nil) if the user hasn't connected Gmail.
func gmailService(ctx context.Context, userUUID uuid.UUID) (*gmail.Service, error) {
	access, err := validAccessToken(ctx, userUUID, ProviderGmail)
	if err != nil {
		return nil, err
	}
	if access == "" {
		return nil, nil
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: access})
	return gmail.NewService(ctx, option.WithTokenSource(ts))
}

// EmailSummary is a compact, AI-friendly view of a message.
type EmailSummary struct {
	ID      string `json:"id"`
	From    string `json:"from"`
	Subject string `json:"subject"`
	Snippet string `json:"snippet"`
	Date    string `json:"date"`
}

// GmailSearch runs a Gmail query (Gmail search syntax) and returns up to
// `limit` compact summaries. Read-only.
func GmailSearch(ctx context.Context, userUUID uuid.UUID, query string, limit int64) ([]EmailSummary, error) {
	svc, err := requireGmail(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 25 {
		limit = 10
	}

	list, err := svc.Users.Messages.List("me").Q(query).MaxResults(limit).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail list: %w", err)
	}

	out := make([]EmailSummary, 0, len(list.Messages))
	for _, m := range list.Messages {
		msg, gerr := svc.Users.Messages.Get("me", m.Id).Format("metadata").
			MetadataHeaders("From", "Subject", "Date").Context(ctx).Do()
		if gerr != nil {
			continue
		}
		es := EmailSummary{ID: m.Id, Snippet: msg.Snippet}
		if msg.Payload != nil {
			hs := msg.Payload.Headers
			es.From, es.Subject, es.Date = header(hs, "From"), header(hs, "Subject"), header(hs, "Date")
		}
		out = append(out, es)
	}
	return out, nil
}

// GmailSend sends an email on the user's behalf. WRITE — only ever invoked
// after explicit user confirmation through the AI ProposedAction gate.
func GmailSend(ctx context.Context, userUUID uuid.UUID, to, subject, body string) (string, error) {
	if strings.TrimSpace(to) == "" {
		return "", ErrNoRecipients
	}
	// The values come from an AI's proposal; rawEmail refuses a line break in
	// any of them.
	raw, err := rawEmail([]mailHeader{{"To", to}, {"Subject", subject}}, body)
	if err != nil {
		return "", err
	}
	svc, err := requireGmail(ctx, userUUID)
	if err != nil {
		return "", err
	}
	sent, err := svc.Users.Messages.Send("me", &gmail.Message{Raw: raw}).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("gmail send: %w", err)
	}
	return sent.Id, nil
}
