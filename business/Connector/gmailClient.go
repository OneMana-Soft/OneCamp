package business

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

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
	svc, err := gmailService(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, ErrNotConnected
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
			for _, h := range msg.Payload.Headers {
				switch h.Name {
				case "From":
					es.From = h.Value
				case "Subject":
					es.Subject = h.Value
				case "Date":
					es.Date = h.Value
				}
			}
		}
		out = append(out, es)
	}
	return out, nil
}

// GmailSend sends an email on the user's behalf. WRITE — only ever invoked
// after explicit user confirmation through the AI ProposedAction gate.
func GmailSend(ctx context.Context, userUUID uuid.UUID, to, subject, body string) (string, error) {
	svc, err := gmailService(ctx, userUUID)
	if err != nil {
		return "", err
	}
	if svc == nil {
		return "", ErrNotConnected
	}
	if strings.TrimSpace(to) == "" {
		return "", fmt.Errorf("recipient is required")
	}
	// A line break in a header value would let it add headers of its own
	// (another recipient, say). The values come from an AI's proposal.
	if hasLineBreak(to) || hasLineBreak(subject) {
		return "", ErrBadHeader
	}

	var sb strings.Builder
	sb.WriteString("To: " + to + "\r\n")
	sb.WriteString("Subject: " + subject + "\r\n")
	sb.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	sb.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	sb.WriteString("\r\n")
	sb.WriteString(body)

	raw := base64.URLEncoding.EncodeToString([]byte(sb.String()))
	sent, err := svc.Users.Messages.Send("me", &gmail.Message{Raw: raw}).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("gmail send: %w", err)
	}
	return sent.Id, nil
}
