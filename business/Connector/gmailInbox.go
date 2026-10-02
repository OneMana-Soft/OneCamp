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
	"net/url"
	"regexp"
	"strings"
	"sync"

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
	svc, err := requireGmail(ctx, userUUID)
	if err != nil {
		return nil, err
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
	errs := make([]error, len(list.Threads))
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
			if gerr != nil {
				errs[i] = gerr
				return
			}
			if len(th.Messages) == 0 {
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
	var firstErr error
	for i := range out {
		if ok[i] {
			page.Threads = append(page.Threads, out[i])
		} else if firstErr == nil && errs[i] != nil {
			firstErr = errs[i]
		}
	}
	// One conversation that will not load is skipped. A page where none did is
	// a failure (a revoked token, say), not an empty inbox.
	if len(page.Threads) == 0 && firstErr != nil {
		return nil, fmt.Errorf("gmail thread: %w", firstErr)
	}
	return page, nil
}

// GmailThread returns one conversation with sanitised bodies.
func GmailThread(ctx context.Context, userUUID uuid.UUID, threadID string) (*InboxThreadDetail, error) {
	if !reThreadID.MatchString(threadID) {
		return nil, ErrBadThreadID
	}
	svc, err := requireGmail(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	// The address names the account the link opens in. Without it, Gmail opens
	// the first account signed in to the browser, which is often the wrong one.
	account := make(chan string, 1)
	go func() {
		p, perr := svc.Users.GetProfile("me").Context(ctx).Do()
		if perr != nil {
			account <- ""
			return
		}
		account <- p.EmailAddress
	}()
	th, err := svc.Users.Threads.Get("me", threadID).Format("full").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail thread: %w", err)
	}
	d := &InboxThreadDetail{ID: threadID, GmailURL: GmailThreadURL(<-account, threadID)}
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

// GmailThreadURL opens a conversation in Gmail, in the given account when known.
func GmailThreadURL(account, threadID string) string {
	if account == "" {
		return "https://mail.google.com/mail/u/0/#all/" + threadID
	}
	return "https://mail.google.com/mail/?authuser=" + url.QueryEscape(account) + "#all/" + threadID
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
	if htmlPart != "" {
		body, truncated := cutBytes(htmlPart, maxBodyBytes)
		return SanitizeEmailHTML(body), truncated
	}
	body, truncated := cutBytes(textPart, maxBodyBytes)
	return PlainToHTML(body), truncated
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

// GmailReply sends body as a reply in the conversation, to the last person who
// wrote to the user (or, if the user wrote last, to whoever they wrote to).
func GmailReply(ctx context.Context, userUUID uuid.UUID, threadID, body string) (string, error) {
	if !reThreadID.MatchString(threadID) {
		return "", ErrBadThreadID
	}
	if strings.TrimSpace(body) == "" {
		return "", ErrEmptyReply
	}
	svc, err := requireGmail(ctx, userUUID)
	if err != nil {
		return "", err
	}
	profile, err := svc.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("gmail profile: %w", err)
	}
	th, err := svc.Users.Threads.Get("me", threadID).Format("metadata").
		MetadataHeaders("From", "To", "Reply-To", "Subject", "Message-ID", "References").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("gmail thread: %w", err)
	}
	if len(th.Messages) == 0 || th.Messages[len(th.Messages)-1].Payload == nil {
		return "", ErrNoRecipient
	}
	hs := th.Messages[len(th.Messages)-1].Payload.Headers
	to := replyRecipient(profile.EmailAddress, header(hs, "From"), header(hs, "Reply-To"), header(hs, "To"))
	if strings.TrimSpace(to) == "" {
		return "", ErrNoRecipient
	}
	subject := header(hs, "Subject")
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	msgID := header(hs, "Message-ID")
	headers := []mailHeader{{"To", to}, {"Subject", subject}}
	if msgID != "" {
		refs := strings.TrimSpace(header(hs, "References") + " " + msgID)
		headers = append(headers, mailHeader{"In-Reply-To", msgID}, mailHeader{"References", refs})
	}
	raw, err := rawEmail(headers, body)
	if err != nil {
		return "", err
	}
	sent, err := svc.Users.Messages.Send("me", &gmail.Message{Raw: raw, ThreadId: threadID}).Context(ctx).Do()
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
