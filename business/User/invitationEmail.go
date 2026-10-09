package business

// The invitation email, from the person who sent it.
//
// It said "Welcome to OneCamp! You've been invited to join", from nobody, to
// nowhere in particular, and the admin was told "an email is on its way"
// whenever a key was set: the send ran in the background and its error was
// dropped. The email now says who invited them and to which workspace, a reply
// reaches that person, and the send is waited for, briefly, so the admin is
// told whether the provider took it and, if not, why (emailService.Reason).

import (
	"context"
	"strings"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	authService "github.com/akashc777/OneCamp/services/Auth"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/google/uuid"
)

// The defaults that shipped (migration 24, the fallback in the code before
// this, and the subject that named the inviter), recognised so a workspace
// that never changed them gets today's wording. One an admin wrote is used as
// written.
var shippedInviteSubjects = []string{"You're invited to OneCamp!", "{{inviter_name}} invited you to OneCamp"}

var shippedInviteTemplates = []string{
	`<h2>Welcome to OneCamp!</h2><p>You've been invited to join. Click the link below to set up your account:</p><p><a href="{{signup_link}}">Accept Invitation</a></p><p>This link expires in 7 days.</p>`,
	`<h2>Welcome to OneCamp!</h2>
{{logo_image}}
<p>You've been invited to join. Click the link below to set up your account:</p>
<p><a href="{{signup_link}}">Accept Invitation</a></p>
<p>This link expires in 7 days.</p>`,
}

// Today's defaults. {{inviter_name}} is who sent it, {{workspace_url}} the
// workspace's address, {{signup_link}} the invitation's link and
// {{logo_image}} the logo set in Admin > Email, or nothing.
//
// The inviter's name is in the body, never the subject. A display name is
// whatever its owner typed, and a subject is what a mail client shows
// unopened: "Your bank" invited you to OneCamp. In a subject,
// {{inviter_name}} is "A teammate" (RenderInvitation).
const (
	DefaultInviteSubject  = "You're invited to join {{workspace_url}} on OneCamp"
	DefaultInviteTemplate = `<h2>{{inviter_name}} invited you to OneCamp</h2>
{{logo_image}}
<p>{{inviter_name}} invited you to join them at {{workspace_url}}.</p>
<p><a href="{{signup_link}}">Accept the invitation</a></p>
<p>The link works for 7 days. Reply to this email to reach {{inviter_name}}.</p>`
)

// sameText compares two templates the way a person reads them: whitespace
// between words is one space, and between tags none. Pure.
func sameText(a, b string) bool {
	read := func(s string) string {
		return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "> <", "><")
	}
	return read(a) == read(b)
}

// InviteSubject is the subject to send: the one an admin wrote, or today's
// default for none or the one that shipped. Pure.
func InviteSubject(stored string) string {
	if strings.TrimSpace(stored) == "" {
		return DefaultInviteSubject
	}
	for _, shipped := range shippedInviteSubjects {
		if sameText(stored, shipped) {
			return DefaultInviteSubject
		}
	}
	return stored
}

// InviteTemplate is the body to send, chosen the same way. Pure.
func InviteTemplate(stored string) string {
	if strings.TrimSpace(stored) == "" {
		return DefaultInviteTemplate
	}
	for _, shipped := range shippedInviteTemplates {
		if sameText(stored, shipped) {
			return DefaultInviteTemplate
		}
	}
	return stored
}

// InviteSender is the address it is sent from: the one an admin chose, or
// this workspace's own sender (emailService.SenderAddress) for none or the
// seeded one, which was OneCamp's own domain and which a workspace's own
// sending key cannot send from. Pure.
func InviteSender(stored, workspaceSender string) string {
	if s := emailService.ChosenSender(stored); s != "" {
		return s
	}
	return workspaceSender
}

// WorkspaceAddress is the workspace's address as people say it: its host,
// without the scheme.
func WorkspaceAddress() string {
	base := authService.FrontendBaseURL()
	if i := strings.Index(base, "://"); i >= 0 {
		base = base[i+3:]
	}
	return strings.TrimRight(base, "/")
}

// InvitationEmail is one invitation to send.
type InvitationEmail struct {
	To    string
	Token string
	// InviterName and InviterEmail are who sent it: named in the email, and
	// where a reply goes.
	InviterName  string
	InviterEmail string
}

// fillTemplate replaces each {{key}} in text. Pure.
func fillTemplate(text string, values map[string]string) string {
	for k, v := range values {
		text = strings.ReplaceAll(text, "{{"+k+"}}", v)
	}
	return text
}

// RenderInvitation is the subject, HTML and plain text of an invitation, from
// the configured (or default) subject and template. Pure.
func RenderInvitation(e InvitationEmail, subject, template, link, workspace, logoHTML string) (string, string, string) {
	inviter := strings.TrimSpace(e.InviterName)
	if inviter == "" {
		inviter = "A teammate"
	}
	html := fillTemplate(template, map[string]string{
		"signup_link":   link,
		"logo_image":    logoHTML,
		"inviter_name":  helpers.EscapeHTML(inviter),
		"workspace_url": helpers.EscapeHTML(workspace),
	})
	// Never the inviter's own name in the subject: see DefaultInviteSubject.
	subj := fillTemplate(subject, map[string]string{
		"inviter_name":  "A teammate",
		"workspace_url": workspace,
		"signup_link":   link,
		"logo_image":    "",
	})
	text := inviter + " invited you to join them on OneCamp at " + workspace + ".\n\n" +
		"Accept the invitation: " + link + "\n\nThe link works for 7 days."
	return strings.TrimSpace(subj), html, text
}

// SendInvitationEmail sends an invitation and returns what the provider said:
// nil when it accepted it. ctx bounds the wait.
func SendInvitationEmail(ctx context.Context, e InvitationEmail) error {
	stored := map[string]string{}
	if rows, err := configModels.GetMultipleConfigsByKeys([]string{
		"sender_email", "invitation_email_subject", "invitation_email_template", "invitation_email_logo",
	}); err == nil {
		for _, row := range rows {
			stored[row.Key] = row.Value
		}
	}
	logo := ""
	if stored["invitation_email_logo"] != "" {
		logo = `<img src="` + authService.BackendBaseURL() + `/public/email/logo" alt="Logo" style="max-height:80px; max-width:200px;" />`
	}
	link := InvitationLink(e.Token)
	subject, html, text := RenderInvitation(e, InviteSubject(stored["invitation_email_subject"]),
		InviteTemplate(stored["invitation_email_template"]), link, WorkspaceAddress(), logo)
	_, err := emailService.SendEmailWithOptions(ctx, emailService.SendOptions{
		From:    InviteSender(stored["sender_email"], emailService.SenderAddress()),
		To:      e.To,
		Subject: subject,
		HTML:    html,
		Text:    text,
		ReplyTo: strings.TrimSpace(e.InviterEmail),
		Tags:    map[string]string{"category": "invitation"},
		// Under a daily cap, invitations leave the last few for password resets.
		Keep: emailService.InvitationReserve,
	})
	if err != nil {
		helpers.LogWarnWithContext(ctx, "business/SendInvitationEmail to %s not sent: %s (%v)", e.To, emailService.Reason(err), err)
	}
	return err
}

// InviterName is the display name of whoever made the invitation, or "" when
// they are gone.
func InviterName(ctx context.Context, invitedBy uuid.UUID) string {
	if invitedBy == uuid.Nil {
		return ""
	}
	inviter, err := domain.GetDgraphUserInfoByUUID(ctx, invitedBy.String())
	if err != nil || inviter == nil {
		return ""
	}
	return NameOnRecord(*inviter)
}

// NameOnRecord is what a person is called where an invitation names them:
// their display name, else their full name, else "". Pure.
func NameOnRecord(u dgraphStruct.DgraphUser) string {
	if name := strings.TrimSpace(u.UserName); name != "" {
		return name
	}
	return strings.TrimSpace(u.UserFullName)
}
