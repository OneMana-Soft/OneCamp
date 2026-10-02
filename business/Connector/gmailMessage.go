package business

import (
	"context"
	"encoding/base64"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/api/gmail/v1"
)

// requireGmail is gmailService for callers that need a connection: not
// connected is ErrNotConnected rather than a nil service.
func requireGmail(ctx context.Context, userUUID uuid.UUID) (*gmail.Service, error) {
	svc, err := gmailService(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, ErrNotConnected
	}
	return svc, nil
}

// mailHeader is one header of an outgoing email. An empty value is left out.
type mailHeader struct{ name, value string }

// addressHeaders hold addresses, whose display names are encoded per address.
var addressHeaders = map[string]bool{"To": true, "Cc": true, "Bcc": true, "Reply-To": true}

// rawEmail builds a plain-text email as Gmail's API takes it (base64url).
//
// Every header value is refused if it carries a line break, because one would
// let it add headers of its own: another recipient, say. Values come from
// Gmail (a reply) or from an AI's proposal (a send), and neither is trusted.
// Non-ASCII subjects and display names are encoded (RFC 2047) so they arrive
// intact rather than as mojibake in stricter mail clients.
func rawEmail(headers []mailHeader, body string) (string, error) {
	var sb strings.Builder
	for _, h := range headers {
		if h.value == "" {
			continue
		}
		if hasLineBreak(h.value) {
			return "", ErrBadHeader
		}
		v := h.value
		switch {
		case h.name == "Subject":
			v = mime.QEncoding.Encode("utf-8", v)
		case addressHeaders[h.name]:
			v = encodeAddressList(v)
		}
		sb.WriteString(h.name + ": " + v + "\r\n")
	}
	sb.WriteString("MIME-Version: 1.0\r\n")
	sb.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	sb.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	sb.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\n")
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	sb.WriteString(body)
	return base64.URLEncoding.EncodeToString([]byte(sb.String())), nil
}

// encodeAddressList re-renders "José <j@x.com>, b@y.com" with encoded display
// names. A list net/mail cannot parse is sent as written: Gmail is lenient, and
// line breaks were already refused.
func encodeAddressList(v string) string {
	list, err := mail.ParseAddressList(v)
	if err != nil {
		return v
	}
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.String()
	}
	return strings.Join(out, ", ")
}

func hasLineBreak(s string) bool { return strings.ContainsAny(s, "\r\n") }

// cutBytes shortens s to at most n bytes without splitting a character.
func cutBytes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	return strings.ToValidUTF8(s[:n], ""), true
}
