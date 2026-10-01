package email

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
)

// TemplateData is the canonical input passed to every notification template.
// We pre-render at enqueue time, so this struct only needs to capture the
// values the template needs to display, not anything dynamic.
type TemplateData struct {
	RecipientName  string
	ActorName      string
	ActorAvatar    string // signed/public URL; can be empty
	Title          string // headline above the message body
	Subtitle       string // contextual line below the headline (e.g. "in #design")
	Body           string // the actual message text (already HTML-escaped or sanitized)
	CTAText        string
	CTAURL         string
	UnsubscribeURL string
	BrandName      string
	BrandLogo      string // optional; rendered if non-empty
	FooterNote     string // optional small line at the bottom
}

// baseTemplate is a single responsive HTML email shell that all event
// templates reuse. Built using table-based layout for Outlook compatibility.
// Inlined CSS only — most clients strip <style> tags.
const baseTemplateHTML = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>{{.Title | html}}</title>
  </head>
  <body style="margin:0; padding:0; background:#f4f5f7; font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,'Helvetica Neue',Arial,sans-serif; color:#0f172a;">
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f4f5f7;">
      <tr>
        <td align="center" style="padding:32px 16px;">
          <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px; background:#ffffff; border-radius:12px; box-shadow:0 1px 3px rgba(15,23,42,0.06); overflow:hidden;">
            {{if .BrandLogo}}
            <tr>
              <td align="center" style="padding:24px 24px 8px 24px;">
                <img src="{{.BrandLogo}}" alt="{{.BrandName | html}}" style="max-height:32px; max-width:160px;" />
              </td>
            </tr>
            {{end}}
            <tr>
              <td style="padding:24px 32px 8px 32px;">
                <h1 style="margin:0 0 4px 0; font-size:18px; font-weight:600; line-height:1.4; color:#0f172a;">{{.Title | html}}</h1>
                {{if .Subtitle}}
                <p style="margin:0; font-size:13px; color:#64748b; line-height:1.5;">{{.Subtitle | html}}</p>
                {{end}}
              </td>
            </tr>

            <tr>
              <td style="padding:16px 32px 8px 32px;">
                <table role="presentation" width="100%" cellpadding="0" cellspacing="0">
                  <tr>
                    {{if .ActorAvatar}}
                    <td width="40" valign="top" style="padding-right:12px;">
                      <img src="{{.ActorAvatar}}" alt="" width="40" height="40" style="border-radius:20px; display:block;" />
                    </td>
                    {{end}}
                    <td valign="top">
                      <div style="font-size:13px; font-weight:600; color:#0f172a; margin-bottom:2px;">{{.ActorName | html}}</div>
                      <div style="font-size:14px; color:#0f172a; line-height:1.55; word-break:break-word;">{{.Body | safehtml}}</div>
                    </td>
                  </tr>
                </table>
              </td>
            </tr>

            {{if .CTAURL}}
            <tr>
              <td align="center" style="padding:24px 32px 32px 32px;">
                <a href="{{.CTAURL}}" style="background:#4F46E5; color:#ffffff; text-decoration:none; padding:10px 20px; border-radius:8px; font-size:14px; font-weight:600; display:inline-block;">
                  {{.CTAText | html}}
                </a>
              </td>
            </tr>
            {{else}}
            <tr><td style="padding-bottom:24px;">&nbsp;</td></tr>
            {{end}}
          </table>

          <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px; margin-top:16px;">
            <tr>
              <td align="center" style="font-size:12px; color:#94a3b8; line-height:1.6;">
                {{if .FooterNote}}<div style="margin-bottom:6px;">{{.FooterNote | html}}</div>{{end}}
                <div>You received this because you're subscribed to notifications on {{.BrandName | html}}.</div>
                {{if .UnsubscribeURL}}
                <div style="margin-top:6px;">
                  <a href="{{.UnsubscribeURL}}" style="color:#94a3b8; text-decoration:underline;">Unsubscribe</a>
                  &nbsp;·&nbsp;
                  <a href="{{.CTAURL}}" style="color:#94a3b8; text-decoration:underline;">View in {{.BrandName | html}}</a>
                </div>
                {{end}}
              </td>
            </tr>
          </table>
        </td>
      </tr>
    </table>
  </body>
</html>`

var baseTpl = template.Must(template.New("email").Funcs(template.FuncMap{
	// html: defensively HTML-escapes the input. Use for any value supplied by
	// a user (sender display name, channel name, etc.).
	"html": func(s string) template.HTML {
		return template.HTML(template.HTMLEscapeString(s))
	},
	// safehtml: trusts the input as already-sanitised HTML. The notification
	// dispatcher strips raw HTML before calling Render and only passes through
	// safe display strings.
	"safehtml": func(s string) template.HTML {
		return template.HTML(s)
	},
}).Parse(baseTemplateHTML))

// Render returns the (HTML, plaintext) pair for a TemplateData. The plain-text
// part is required by every modern mailbox provider for deliverability.
func Render(data TemplateData) (string, string, error) {
	if data.BrandName == "" {
		data.BrandName = "OneCamp"
	}
	if data.CTAText == "" && data.CTAURL != "" {
		data.CTAText = "Open in " + data.BrandName
	}

	var buf bytes.Buffer
	if err := baseTpl.Execute(&buf, data); err != nil {
		return "", "", fmt.Errorf("render html: %w", err)
	}
	return buf.String(), renderPlainText(data), nil
}

// renderPlainText hand-builds a no-frills text/plain alternative. We don't
// use a separate template here because the structure is trivially expressible
// as concatenation, and keeping it inlined avoids template-parsing overhead
// per send.
func renderPlainText(d TemplateData) string {
	var b strings.Builder
	if d.BrandName != "" {
		b.WriteString(d.BrandName + "\n")
		b.WriteString(strings.Repeat("=", len(d.BrandName)) + "\n\n")
	}
	if d.Title != "" {
		b.WriteString(d.Title + "\n\n")
	}
	if d.Subtitle != "" {
		b.WriteString(d.Subtitle + "\n\n")
	}
	if d.ActorName != "" {
		b.WriteString(d.ActorName + ":\n")
	}
	if d.Body != "" {
		// Strip HTML tags from the body for the plaintext version.
		b.WriteString(stripHTMLTags(d.Body) + "\n\n")
	}
	if d.CTAURL != "" {
		if d.CTAText != "" {
			b.WriteString(d.CTAText + ": ")
		}
		b.WriteString(d.CTAURL + "\n\n")
	}
	b.WriteString("---\n")
	if d.FooterNote != "" {
		b.WriteString(d.FooterNote + "\n")
	}
	if d.UnsubscribeURL != "" {
		b.WriteString("Unsubscribe: " + d.UnsubscribeURL + "\n")
	}
	return b.String()
}

// stripHTMLTags is a small dependency-free tag stripper. Good enough for the
// notification body which is plain text rendered into <span>s by the template.
func stripHTMLTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return strings.TrimSpace(b.String())
}
