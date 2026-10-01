package business

// The inbox: a person's own Gmail inside OneCamp, so email and the work it
// turns into live in one place.
//
// Read-only on the mailbox except for replies the person writes and sends
// themselves. It uses the Gmail connection the person made (gmail.readonly and
// gmail.send), so reading here does not mark messages read in Gmail.
//
// Email is hostile input. Bodies are sanitised to a small HTML subset, and
// remote images are dropped: an image URL is how a sender learns an email was
// opened, and when.

import (
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/microcosm-cc/bluemonday"
	"google.golang.org/api/gmail/v1"
)

const (
	inboxPageSize   = 25
	inboxFetchWidth = 8
	// maxBodyBytes bounds one message body sent to the browser. A newsletter can
	// be megabytes of markup; the person can still open it in Gmail.
	maxBodyBytes = 300 << 10
)

// ErrBadHeader is a recipient or subject carrying a line break, which would
// let it add headers of its own to the message.
var ErrBadHeader = fmt.Errorf("an address or subject cannot contain a line break")

// InboxThread is one conversation in the list.
type InboxThread struct {
	ID       string `json:"id"`
	Subject  string `json:"subject"`
	From     string `json:"from"`
	Snippet  string `json:"snippet"`
	Date     string `json:"date"`
	Unread   bool   `json:"unread"`
	Messages int    `json:"messages"`
}

// InboxPage is one page of conversations.
type InboxPage struct {
	Threads       []InboxThread `json:"threads"`
	NextPageToken string        `json:"next_page_token,omitempty"`
}

// InboxMessage is one email in a conversation. Body is sanitised HTML.
type InboxMessage struct {
	ID          string `json:"id"`
	From        string `json:"from"`
	To          string `json:"to"`
	Cc          string `json:"cc,omitempty"`
	Date        string `json:"date"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	Truncated   bool   `json:"truncated,omitempty"`
	Attachments int    `json:"attachments,omitempty"`
}

// InboxThreadDetail is a whole conversation.
type InboxThreadDetail struct {
	ID       string         `json:"id"`
	Subject  string         `json:"subject"`
	Messages []InboxMessage `json:"messages"`
	// GmailURL opens the conversation in Gmail, for attachments and anything
	// else this view does not do.
	GmailURL string `json:"gmail_url"`
}

var reThreadID = regexp.MustCompile(`^[A-Za-z0-9]{6,64}$`)

func header(hs []*gmail.MessagePartHeader, name string) string {
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// GmailInbox lists the person's conversations. query is Gmail search syntax;
// empty means the inbox.
func GmailInbox(ctx context.Context, userUUID uuid.UUID, query, pageToken string) (*InboxPage, error) {
	svc, err := gmailService(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, ErrNotConnected
	}
	q := strings.TrimSpace(query)
	if q == "" {
		q = "in:inbox"
	}
	call := svc.Users.Threads.List("me").Q(q).MaxResults(inboxPageSize).Context(ctx)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	list, err := call.Do()
	if err != nil {
		return nil, fmt.Errorf("gmail threads: %w", err)
	}

	out := make([]InboxThread, len(list.Threads))
	ok := make([]bool, len(list.Threads))
	var wg sync.WaitGroup
	sem := make(chan struct{}, inboxFetchWidth)
	for i, t := range list.Threads {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			th, gerr := svc.Users.Threads.Get("me", id).Format("metadata").
				MetadataHeaders("From", "Subject", "Date").Context(ctx).Do()
			if gerr != nil || len(th.Messages) == 0 {
				return
			}
			first, last := th.Messages[0], th.Messages[len(th.Messages)-1]
			it := InboxThread{ID: id, Snippet: html.UnescapeString(last.Snippet), Messages: len(th.Messages)}
			if first.Payload != nil {
				it.Subject = header(first.Payload.Headers, "Subject")
				it.From = header(first.Payload.Headers, "From")
			}
			if last.Payload != nil {
				it.Date = header(last.Payload.Headers, "Date")
			}
			for _, m := range th.Messages {
				for _, l := range m.LabelIds {
					if l == "UNREAD" {
						it.Unread = true
					}
				}
			}
			out[i], ok[i] = it, true
		}(i, t.Id)
	}
	wg.Wait()

	page := &InboxPage{Threads: make([]InboxThread, 0, len(out)), NextPageToken: list.NextPageToken}
	for i := range out {
		if ok[i] {
			page.Threads = append(page.Threads, out[i])
		}
	}
	return page, nil
}

// GmailThread returns one conversation with sanitised bodies.
func GmailThread(ctx context.Context, userUUID uuid.UUID, threadID string) (*InboxThreadDetail, error) {
	if !reThreadID.MatchString(threadID) {
		return nil, fmt.Errorf("not a conversation id")
	}
	svc, err := gmailService(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	if svc == nil {
		return nil, ErrNotConnected
	}
	th, err := svc.Users.Threads.Get("me", threadID).Format("full").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail thread: %w", err)
	}
	d := &InboxThreadDetail{ID: threadID, GmailURL: "https://mail.google.com/mail/u/0/#all/" + threadID}
	for _, m := range th.Messages {
		if m.Payload == nil {
			continue
		}
		hs := m.Payload.Headers
		body, truncated := messageBody(m.Payload)
		im := InboxMessage{
			ID: m.Id, From: header(hs, "From"), To: header(hs, "To"), Cc: header(hs, "Cc"),
			Date: header(hs, "Date"), Subject: header(hs, "Subject"),
			Body: body, Truncated: truncated, Attachments: countAttachments(m.Payload),
		}
		if d.Subject == "" {
			d.Subject = im.Subject
		}
		d.Messages = append(d.Messages, im)
	}
	return d, nil
}

// emailPolicy is the HTML an email body may keep: text formatting, lists,
// tables and links. No images from the network, no styles that can position
// content over the app, no forms or scripts.
var emailPolicy = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowDataURIImages()
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	return p
}()

var reRemoteImg = regexp.MustCompile(`(?is)<img\b[^>]*\bsrc\s*=\s*["']?\s*(https?:)?//[^>]*>`)

// SanitizeEmailHTML makes an email body safe to show inside OneCamp. Exported
// for its test.
func SanitizeEmailHTML(raw string) string {
	return emailPolicy.Sanitize(reRemoteImg.ReplaceAllString(raw, ""))
}

// PlainToHTML shows a plain-text body as escaped paragraphs.
func PlainToHTML(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return "<p>" + strings.ReplaceAll(html.EscapeString(text), "\n", "<br/>") + "</p>"
}

// messageBody prefers the HTML part (sanitised), then plain text.
func messageBody(p *gmail.MessagePart) (string, bool) {
	var htmlPart, textPart string
	var walk func(*gmail.MessagePart)
	walk = func(part *gmail.MessagePart) {
		if part == nil {
			return
		}
		if part.Filename == "" && part.Body != nil && part.Body.Data != "" {
			switch {
			case strings.HasPrefix(part.MimeType, "text/html") && htmlPart == "":
				htmlPart = decodePart(part.Body.Data)
			case strings.HasPrefix(part.MimeType, "text/plain") && textPart == "":
				textPart = decodePart(part.Body.Data)
			}
		}
		for _, c := range part.Parts {
			walk(c)
		}
	}
	walk(p)
	truncated := false
	cut := func(s string) string {
		if len(s) > maxBodyBytes {
			truncated = true
			return s[:maxBodyBytes]
		}
		return s
	}
	if htmlPart != "" {
		return SanitizeEmailHTML(cut(htmlPart)), truncated
	}
	return PlainToHTML(cut(textPart)), truncated
}

func decodePart(data string) string {
	b, err := base64.URLEncoding.DecodeString(data)
	if err != nil {
		b, err = base64.RawURLEncoding.DecodeString(data)
		if err != nil {
			return ""
		}
	}
	return string(b)
}

func countAttachments(p *gmail.MessagePart) int {
	if p == nil {
		return 0
	}
	n := 0
	if p.Filename != "" {
		n++
	}
	for _, c := range p.Parts {
		n += countAttachments(c)
	}
	return n
}

// ThreadText is a conversation as plain text, for an AI summary.
func ThreadText(d *InboxThreadDetail) string {
	var b strings.Builder
	b.WriteString("Subject: " + d.Subject + "\n\n")
	for _, m := range d.Messages {
		fmt.Fprintf(&b, "From: %s\nDate: %s\n\n%s\n\n---\n\n", m.From, m.Date, htmlToText(m.Body))
	}
	return b.String()
}

var reTags = regexp.MustCompile(`(?s)<[^>]*>`)

func htmlToText(s string) string {
	s = strings.NewReplacer("<br/>", "\n", "<br>", "\n", "</p>", "\n", "</div>", "\n").Replace(s)
	return strings.TrimSpace(html.UnescapeString(reTags.ReplaceAllString(s, "")))
}

func hasLineBreak(s string) bool { return strings.ContainsAny(s, "\r\n") }

// GmailReply sends body as a reply in the conversation, to the last person who
// wrote to the user (or, if the user wrote last, to whoever they wrote to).
func GmailReply(ctx context.Context, userUUID uuid.UUID, threadID, body string) (string, error) {
	if !reThreadID.MatchString(threadID) {
		return "", fmt.Errorf("not a conversation id")
	}
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("write a reply first")
	}
	svc, err := gmailService(ctx, userUUID)
	if err != nil {
		return "", err
	}
	if svc == nil {
		return "", ErrNotConnected
	}
	profile, err := svc.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("gmail profile: %w", err)
	}
	th, err := svc.Users.Threads.Get("me", threadID).Format("metadata").
		MetadataHeaders("From", "To", "Reply-To", "Subject", "Message-ID", "References").Context(ctx).Do()
	if err != nil || len(th.Messages) == 0 {
		return "", fmt.Errorf("gmail thread: %v", err)
	}
	last := th.Messages[len(th.Messages)-1]
	hs := last.Payload.Headers
	to := replyRecipient(profile.EmailAddress, header(hs, "From"), header(hs, "Reply-To"), header(hs, "To"))
	subject := header(hs, "Subject")
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	msgID := header(hs, "Message-ID")
	refs := strings.TrimSpace(header(hs, "References") + " " + msgID)
	for _, v := range []string{to, subject, msgID, refs} {
		if hasLineBreak(v) {
			return "", ErrBadHeader
		}
	}
	if to == "" {
		return "", fmt.Errorf("could not tell who to reply to")
	}
	var sb strings.Builder
	sb.WriteString("To: " + to + "\r\n")
	sb.WriteString("Subject: " + subject + "\r\n")
	if msgID != "" {
		sb.WriteString("In-Reply-To: " + msgID + "\r\n")
		sb.WriteString("References: " + refs + "\r\n")
	}
	sb.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	sb.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\n")
	sb.WriteString(body)
	sent, err := svc.Users.Messages.Send("me", &gmail.Message{
		Raw:      base64.URLEncoding.EncodeToString([]byte(sb.String())),
		ThreadId: threadID,
	}).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("gmail send: %w", err)
	}
	return sent.Id, nil
}

// replyRecipient picks who a reply goes to. Pure, for its test.
func replyRecipient(me, from, replyTo, to string) string {
	sender := from
	if replyTo != "" {
		sender = replyTo
	}
	if a, err := mail.ParseAddress(from); err == nil && strings.EqualFold(a.Address, me) {
		// The user wrote last: reply goes to the same people again.
		return to
	}
	return sender
}
