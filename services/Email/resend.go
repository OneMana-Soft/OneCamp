package email

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
)

const resendAPIURL = "https://api.resend.com/emails"

// SharedHTTPClient is the single client used for every outbound Resend call.
// Reusing it lets the Go runtime pool TLS sessions and TCP connections,
// which matters when the worker drains a queue burst.
var SharedHTTPClient = &http.Client{Timeout: 15 * time.Second}

// IsEmailEnabled is the global feature flag. The whole notification email
// system is a no-op when this returns false: the dispatcher, queue inserts,
// worker, and digest scheduler all check it. An operator who deploys
// OneCamp without setting RESEND_API_KEY gets exactly zero email-related
// behaviour: no rows inserted, no goroutines started, no log noise beyond
// a one-time startup notice.
func IsEmailEnabled() bool {
	return strings.TrimSpace(os.Getenv("RESEND_API_KEY")) != ""
}

// SenderAddress returns the configured sender email address (from system
// configs or env fallback). Avoids emitting invalid addresses like "noreply@"
// when FE_DOMAIN is empty.
func SenderAddress() string {
	if sender := strings.TrimSpace(os.Getenv("SENDER_EMAIL")); sender != "" {
		return sender
	}
	domain := strings.TrimSpace(os.Getenv("FE_DOMAIN"))
	if domain == "" {
		domain = strings.TrimSpace(os.Getenv("FRONTEND_DOMAIN"))
	}
	if domain == "" {
		domain = "example.com"
	}
	// Strip scheme if someone put a full URL in the env var.
	if strings.HasPrefix(domain, "http://") || strings.HasPrefix(domain, "https://") {
		if idx := strings.Index(domain, "://"); idx >= 0 {
			domain = domain[idx+3:]
		}
	}
	if strings.Contains(domain, ":") {
		parts := strings.Split(domain, ":")
		if len(parts) > 0 {
			domain = parts[0]
		}
	}
	return "noreply@" + domain
}

// SendOptions extends the existing fire-and-forget Send with notification
// metadata used by the worker (List-Unsubscribe header, plain-text part,
// idempotency key, custom headers).
type SendOptions struct {
	From                string
	To                  string
	Subject             string
	HTML                string
	Text                string
	IdempotencyKey      string
	UnsubscribeURL      string
	UnsubscribeMailto   string
	ListUnsubscribePost bool // adds List-Unsubscribe-Post for one-click (RFC 8058)
	ReplyTo             string
	Tags                map[string]string
}

// SendResult is what callers use to record the delivery outcome.
type SendResult struct {
	MessageID string
}

type resendPayload struct {
	From    string            `json:"from"`
	To      []string          `json:"to"`
	Subject string            `json:"subject"`
	HTML    string            `json:"html"`
	Text    string            `json:"text,omitempty"`
	ReplyTo string            `json:"reply_to,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Tags    []resendTag       `json:"tags,omitempty"`
}

type resendTag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resendResponse struct {
	ID      string `json:"id"`
	Message string `json:"message,omitempty"`
}

// senderTokenBucket throttles the absolute send rate so a queue-drain spike
// does not breach Resend's per-second budget. Configurable via
// RESEND_RATE_PER_SECOND (default 10/s).
//
// The bucket is **lazily** initialised on first use rather than at package-
// init time. Package-level `var x = f()` runs before main.init() has loaded
// the `.env` file, so reading env vars during package init would silently
// ignore values supplied via the dotfile. Lazy init avoids that gotcha.
type senderTokenBucket struct {
	mu     sync.Mutex
	once   sync.Once
	last   time.Time
	tokens float64
	max    float64
	refill float64
}

// resolveRate loads RESEND_RATE_PER_SECOND once and clamps to a sane range.
// Default 10 messages/second matches Resend's standard tier.
func (b *senderTokenBucket) resolveRate() {
	rate := 10.0
	if v := strings.TrimSpace(os.Getenv("RESEND_RATE_PER_SECOND")); v != "" {
		var f float64
		if _, err := fmt.Sscanf(v, "%f", &f); err == nil && f > 0 && f <= 1000 {
			rate = f
		}
	}
	b.last = time.Now()
	b.tokens = rate
	b.max = rate
	b.refill = rate
}

var globalBucket = &senderTokenBucket{}

// take blocks until a token is available, or the supplied context is
// cancelled. Returns ctx.Err() in the cancellation case. Never returns nil
// without consuming a token.
func (b *senderTokenBucket) take(ctx context.Context) error {
	b.once.Do(b.resolveRate)
	for {
		b.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(b.last).Seconds()
		b.tokens += elapsed * b.refill
		if b.tokens > b.max {
			b.tokens = b.max
		}
		b.last = now
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		needed := 1 - b.tokens
		wait := time.Duration(needed/b.refill*float64(time.Second)) + 5*time.Millisecond
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// SendEmail keeps the legacy fire-and-forget signature for the existing
// password-reset and invitation flows. Internally it delegates to SendEmailWithOptions
// so the new throttling and retry behaviour applies to those paths too.
func SendEmail(ctx context.Context, from string, to string, subject string, htmlBody string, textBody ...string) error {
	opts := SendOptions{
		From:    from,
		To:      to,
		Subject: subject,
		HTML:    htmlBody,
	}
	if len(textBody) > 0 && textBody[0] != "" {
		opts.Text = textBody[0]
	}
	_, err := SendEmailWithOptions(ctx, opts)
	return err
}

// SendEmailWithOptions performs a single Resend API call honouring rate
// limits and basic deliverability headers. Callers that need retries should
// wrap this and persist their state in the email queue (the worker does this).
//
// Returns an error when RESEND_API_KEY is empty so callers can surface the
// failure visibly. The notification worker already gates on IsEmailEnabled();
// direct callers (password-reset, invitation) must handle the error.
func SendEmailWithOptions(ctx context.Context, opt SendOptions) (SendResult, error) {
	apiKey := strings.TrimSpace(os.Getenv("RESEND_API_KEY"))
	if apiKey == "" {
		return SendResult{}, errors.New("RESEND_API_KEY is not set: transactional email is disabled")
	}
	if opt.From == "" || opt.To == "" || opt.Subject == "" {
		return SendResult{}, errors.New("from/to/subject are required")
	}
	if opt.HTML == "" {
		return SendResult{}, errors.New("html body is required")
	}

	// Validate email addresses before wasting a Resend API call. Malformed
	// addresses like "noreply@" (empty domain) produce a 422 that is hard
	// to diagnose in production.
	if _, err := mail.ParseAddress(opt.From); err != nil {
		return SendResult{}, fmt.Errorf("invalid sender email %q: %w", opt.From, err)
	}
	if _, err := mail.ParseAddress(opt.To); err != nil {
		return SendResult{}, fmt.Errorf("invalid recipient email %q: %w", opt.To, err)
	}

	// Counted after validation, so a malformed request spends none of the day.
	if err := sentToday.take(time.Now(), dailyCapFromEnv()); err != nil {
		return SendResult{}, err
	}
	if err := globalBucket.take(ctx); err != nil {
		return SendResult{}, err
	}

	payload := resendPayload{
		From:    opt.From,
		To:      []string{opt.To},
		Subject: opt.Subject,
		HTML:    opt.HTML,
		Text:    opt.Text,
		ReplyTo: opt.ReplyTo,
	}
	headers := map[string]string{}
	if opt.UnsubscribeURL != "" {
		// RFC 8058 List-Unsubscribe header. The mailto: leg is required by
		// most mailbox providers as a fallback alongside the URL leg.
		parts := []string{}
		if opt.UnsubscribeMailto != "" {
			parts = append(parts, "<mailto:"+opt.UnsubscribeMailto+">")
		}
		parts = append(parts, "<"+opt.UnsubscribeURL+">")
		headers["List-Unsubscribe"] = strings.Join(parts, ", ")
		if opt.ListUnsubscribePost {
			headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
		}
	}
	if opt.IdempotencyKey != "" {
		headers["Idempotency-Key"] = opt.IdempotencyKey
	}
	if len(headers) > 0 {
		payload.Headers = headers
	}
	for k, v := range opt.Tags {
		payload.Tags = append(payload.Tags, resendTag{Name: k, Value: v})
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return SendResult{}, fmt.Errorf("failed to marshal email payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resendAPIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return SendResult{}, fmt.Errorf("failed to create email request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if opt.IdempotencyKey != "" {
		// Resend docs accept this in the body header as well; setting both is
		// safe and ensures idempotency on transport-layer retries.
		req.Header.Set("Idempotency-Key", opt.IdempotencyKey)
	}

	resp, err := SharedHTTPClient.Do(req)
	if err != nil {
		return SendResult{}, fmt.Errorf("failed to send email: %w", err)
	}
	defer resp.Body.Close()

	var respBody bytes.Buffer
	_, _ = respBody.ReadFrom(resp.Body)

	if resp.StatusCode >= 400 {
		// Treat 401/403/404 (auth or domain misconfig) and 422 (invalid
		// payload) as terminal so the worker can give up. 429 / 5xx are
		// transient and the worker should back off + retry.
		errStr := fmt.Sprintf("resend API returned status %d: %s", resp.StatusCode, respBody.String())
		helpers.LogErrorWithContext(ctx,
			"services/Email/SendEmailWithOptions failed status=%d body=%s", resp.StatusCode, respBody.String())
		return SendResult{}, &SendError{
			StatusCode: resp.StatusCode,
			Message:    errStr,
			Terminal:   isTerminalStatus(resp.StatusCode),
		}
	}

	var parsed resendResponse
	_ = json.Unmarshal(respBody.Bytes(), &parsed)
	helpers.LogInfoWithContext(ctx,
		"services/Email/SendEmailWithOptions sent to=%s subject=%q id=%s", opt.To, opt.Subject, parsed.ID)

	return SendResult{MessageID: parsed.ID}, nil
}

// SendError wraps a non-2xx Resend response so callers can decide whether
// to retry or give up.
type SendError struct {
	StatusCode int
	Message    string
	Terminal   bool
}

func (e *SendError) Error() string { return e.Message }

// IsTerminal reports whether a sender error should stop retries.
func IsTerminal(err error) bool {
	var se *SendError
	if errors.As(err, &se) {
		return se.Terminal
	}
	return false
}

func isTerminalStatus(code int) bool {
	switch code {
	case http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// SendInvitationEmail sends an invitation email with the signup link.
// Kept for backward compatibility with the existing user-controller flow.
func SendInvitationEmail(ctx context.Context, to string, senderEmail string, subject string, template string, signupLink string) error {
	htmlBody := strings.ReplaceAll(template, "{{signup_link}}", signupLink)
	return SendEmail(ctx, senderEmail, to, subject, htmlBody)
}

// SendPasswordResetEmail sends a password reset email.
func SendPasswordResetEmail(ctx context.Context, to string, senderEmail string, resetLink string) error {
	subject := "Reset your OneCamp password"
	htmlBody := fmt.Sprintf(`
		<div style="font-family: sans-serif; max-width: 600px; margin: 0 auto;">
			<h2>Reset Your Password</h2>
			<p>We received a request to reset your OneCamp password. Click the button below to set a new password:</p>
			<p style="text-align: center; margin: 30px 0;">
				<a href="%s" style="background-color: #4F46E5; color: white; padding: 12px 24px; text-decoration: none; border-radius: 6px; display: inline-block; font-weight: 600;">
					Reset Password
				</a>
			</p>
			<p style="color: #666; font-size: 14px;">This link expires in 1 hour. If you didn't request this, you can safely ignore this email.</p>
			<hr style="border: none; border-top: 1px solid #eee; margin: 30px 0;" />
			<p style="color: #999; font-size: 12px;">OneCamp — Your team collaboration platform</p>
		</div>
	`, resetLink)

	textBody := fmt.Sprintf("Reset your OneCamp password\n\nWe received a request to reset your password. Open the link below to set a new password:\n\n%s\n\nThis link expires in 1 hour. If you didn't request this, you can safely ignore this email.", resetLink)

	return SendEmail(ctx, senderEmail, to, subject, htmlBody, textBody)
}
