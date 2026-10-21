package business

// The HTML documents a published page's address answers with (D-25): the page
// itself, a password page's way in, and the calm pages for an address with no
// page, an expired page and a busy one. Script-free and self-contained: no
// font, no stylesheet, nothing from anywhere else; a style of their own,
// light or dark as the reader's device is. The 404 never says whether a doc
// exists (condition 7).

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"html/template"
	"net/http"
	"strconv"
	"time"
)

// Document is one answer: its status, its bytes, and how it may be kept.
type Document struct {
	Status int
	Body   []byte
	// ETag names a page's bytes; "" for an answer nobody keeps.
	ETag string
	// Indexable is a page its publisher let search engines find.
	Indexable bool
	// Revalidate is a page a cache may keep only if it asks first (no-cache):
	// unpublishing then ends it everywhere at once. Every other answer is
	// kept by nobody (no-store).
	Revalidate bool
	// Private is a password page's: no shared cache may keep it, even asking.
	Private bool
}

// galleryCovers are the covers people pick from the gallery, as the web app
// draws them (chrome/cover.css), by name.
var galleryCovers = map[string]bool{
	"sky": true, "lake": true, "moss": true, "sun": true, "berry": true,
	"dusk": true, "dawn": true, "tide": true, "paper": true, "ink": true,
}

// style is the documents' own: a reading column, the device's fonts, light or
// dark, and the camp's colours for the gallery's covers.
const style = `:root{color-scheme:light dark;--bg:#fbfbfa;--ink:#1d1d1b;--ink2:#5b5b57;--line:#e7e6e2;--soft:#f2f1ee;--link:#2f5fd0;--ok:#2b7a4b;` +
	`--sun:#B98200;--sun-t:#FFF6D6;--moss:#2F9E5B;--moss-t:#E6F6EC;--lake:#0E97A6;--lake-t:#E1F5F6;--sky:#3B7DDD;--sky-t:#E7EFFC;--dusk:#7B61D9;--dusk-t:#EFEBFC;--berry:#D9467C;--berry-t:#FCE8EF;` +
	`--sun-i:#7A5200;--moss-i:#1D6A3C;--lake-i:#0B5C65;--sky-i:#1E4E9A;--dusk-i:#4B3699;--berry-i:#962556;--press:rgba(20,22,30,.07)}
@media (prefers-color-scheme:dark){:root{--bg:#161615;--ink:#ecebe7;--ink2:#a5a49f;--line:#2c2c2a;--soft:#1f1f1d;--link:#8fb0ff;--ok:#7fd1a0;` +
	`--sun:#F5C542;--sun-t:#2E2712;--moss:#4CC38A;--moss-t:#14291F;--lake:#3CC6D2;--lake-t:#10292F;--sky:#6EA4F5;--sky-t:#152238;--dusk:#A795F5;--dusk-t:#211B38;--berry:#F07AA6;--berry-t:#33161F;` +
	`--sun-i:#FBE29C;--moss-i:#A6E7C4;--lake-i:#A3E6EC;--sky-i:#B9D3FB;--dusk-i:#D4CBFB;--berry-i:#F9C2D6;--press:rgba(255,255,255,.07)}}
*{box-sizing:border-box}html{-webkit-text-size-adjust:100%}
body{margin:0;background:var(--bg);color:var(--ink);font:16px/1.7 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif}
body.serif main{font-family:ui-serif,Georgia,Cambria,"Times New Roman",serif}
body.mono main{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
body.small article{font-size:14px}
main{max-width:720px;margin:0 auto;padding:56px 20px 72px}body.wide main{max-width:1100px}
.cover{height:30vh;max-height:280px;min-height:120px;background:var(--soft)}
.cover img{display:block;width:100%;height:100%;object-fit:cover;border-radius:0}
.cover-sky{background:linear-gradient(120deg,var(--sky) 0%,var(--lake) 100%)}
.cover-dusk{background:linear-gradient(120deg,var(--dusk) 0%,var(--berry) 100%)}
.cover-moss{background:linear-gradient(120deg,var(--moss) 0%,var(--lake) 100%)}
.cover-sun{background:linear-gradient(120deg,var(--sun) 0%,var(--berry) 100%)}
.cover-berry{background:linear-gradient(120deg,var(--berry) 0%,var(--dusk) 100%)}
.cover-lake{background:linear-gradient(120deg,var(--lake) 0%,var(--sky) 100%)}
.cover-dawn{background:linear-gradient(120deg,var(--sun-t) 0%,var(--berry-t) 50%,var(--dusk-t) 100%)}
.cover-tide{background:linear-gradient(120deg,var(--sky-t) 0%,var(--lake-t) 50%,var(--moss-t) 100%)}
.cover-paper{background-color:var(--soft);background-image:radial-gradient(var(--line) 1px,transparent 1px);background-size:14px 14px}
.cover-ink{background:linear-gradient(120deg,var(--ink2) 0%,var(--ink) 100%)}
.icon{font-size:3.2rem;line-height:1;margin:0 0 12px}.cover+main .icon{margin-top:-28px}.cover+main .fresh+article .icon{margin-top:0}
h1.title{font-size:2.1rem;line-height:1.2;letter-spacing:-.02em;margin:0 0 28px}
article h1{font-size:1.75rem}article h2{font-size:1.4rem}article h3{font-size:1.15rem}
article h1,article h2,article h3,article h4{line-height:1.3;margin:1.8em 0 .5em;letter-spacing:-.01em}
p{margin:.6em 0}a{color:var(--link);text-underline-offset:2px}
img{max-width:100%;height:auto;border-radius:8px}
pre,code{font:.9em/1.6 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
pre{background:var(--soft);padding:14px 16px;border-radius:8px;overflow:auto}
:not(pre)>code{background:var(--soft);padding:.1em .35em;border-radius:4px}
blockquote{margin:1em 0;padding:.1em 1em;border-left:3px solid var(--line);color:var(--ink2)}
hr{border:0;border-top:1px solid var(--line);margin:2em 0}
table{border-collapse:collapse;width:100%;margin:1em 0;display:block;overflow:auto}
th,td{border:1px solid var(--line);padding:6px 10px;text-align:left;vertical-align:top}th{background:var(--soft)}
.embed{display:flex;flex-wrap:wrap;align-items:baseline;gap:4px 12px;border:1px solid var(--line);border-radius:10px;padding:12px 16px;margin:1em 0}.embed .es{font-weight:600}.embed .eh{flex:1;min-width:0;color:var(--ink2);font-size:.85rem;overflow-wrap:anywhere}.embed a{font-weight:500}
.callout{display:flex;gap:10px;background:var(--soft);border-radius:10px;padding:12px 16px;margin:1em 0}.callout>div{flex:1;min-width:0}
.callout .ci{font-size:1.2em;line-height:1.6}.callout>div>:first-child{margin-top:0}.callout>div>:last-child{margin-bottom:0}
.columns{display:flex;gap:24px;flex-wrap:wrap}.column{flex:1 1 220px;min-width:0}
details.toggle{margin:.6em 0}details.toggle>summary{cursor:pointer}details.toggle>summary>:is(h1,h2,h3,h4,h5,h6){display:inline;margin:0}
mark{background:var(--sun-t);color:inherit;border-radius:3px;padding:0 .1em}
.c-gray,.bc-gray{color:var(--ink2)}.c-sun,.bc-sun{color:var(--sun-i)}.c-moss,.bc-moss{color:var(--moss-i)}.c-lake,.bc-lake{color:var(--lake-i)}
.c-sky,.bc-sky{color:var(--sky-i)}.c-dusk,.bc-dusk{color:var(--dusk-i)}.c-berry,.bc-berry{color:var(--berry-i)}
.h-gray,.bg-gray{background:var(--press)}.h-sun,.bg-sun{background:var(--sun-t)}.h-moss,.bg-moss{background:var(--moss-t)}.h-lake,.bg-lake{background:var(--lake-t)}
.h-sky,.bg-sky{background:var(--sky-t)}.h-dusk,.bg-dusk{background:var(--dusk-t)}.h-berry,.bg-berry{background:var(--berry-t)}
:is(p,h1,h2,h3,h4,h5,h6,blockquote,li,details)[class*="bg-"]{border-radius:6px;padding:2px 6px}
pre.diff .da{color:var(--moss-i);background:var(--moss-t)}pre.diff .dd{color:var(--berry-i);background:var(--berry-t)}pre.diff .dm{color:var(--ink2)}
ul.tasks{list-style:none;padding-left:0}li.task{display:flex;gap:.55em;align-items:baseline;margin:.25em 0}li.task>p{margin:0}
li.task .box{flex:none;color:var(--ink2);font-size:1.1em}li.task.done{color:var(--ink2)}li.task.done>:not(.box){text-decoration:line-through}
li.task.done .box{color:var(--ok)}
code.math{white-space:pre-wrap}
.oc-hidden{color:var(--ink2);font-size:.9em;font-style:italic}
footer{margin-top:56px;padding-top:16px;border-top:1px solid var(--line);color:var(--ink2);font-size:.85rem}
.fresh{margin:0 0 28px;color:var(--ink2);font-size:.85rem}.fresh a{color:inherit}
.notice{max-width:440px;padding-top:12vh}.ncard{border:1px solid var(--line);border-radius:16px;padding:28px 28px 10px;background:var(--bg)}
.notice h1{font-size:1.375rem;line-height:1.25;margin:0 0 6px;letter-spacing:-.01em}.notice p{color:var(--ink2);margin:0 0 18px}
.mark{display:flex;align-items:center;justify-content:center;width:48px;height:48px;border-radius:14px;background:var(--soft);color:var(--ink2);margin:0 0 18px}.mark svg{width:24px;height:24px}
.made{display:flex;justify-content:center;margin:20px 0 0}.made span{display:inline-flex;align-items:center;height:28px;padding:0 12px;border-radius:999px;border:1px solid var(--line);color:var(--ink2);font-size:.75rem;font-weight:500}
a.action{display:inline-flex;align-items:center;height:38px;padding:0 16px;border-radius:999px;background:var(--ink);color:var(--bg);font-weight:600;text-decoration:none}
a.action:hover{opacity:.9}a.action:focus-visible{outline:2px solid var(--link);outline-offset:2px}`

// StyleSource is the Content-Security-Policy source that lets the documents'
// own style, and nothing else, apply: its hash.
var StyleSource = func() string {
	sum := sha256.Sum256([]byte(style))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}()

var documents = template.Must(template.New("").Parse(`
{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="{{if .Indexable}}index, follow{{else}}noindex, nofollow{{end}}">
<meta name="referrer" content="no-referrer">
<title>{{.Title}}</title>{{if .Canonical}}
<link rel="canonical" href="{{.Canonical}}">{{end}}
<style>{{.Style}}</style></head>{{end}}
{{define "page"}}{{template "head" .}}
<body{{if .BodyClass}} class="{{.BodyClass}}"{{end}}>{{if .CoverURL}}<div class="cover"><img src="{{.CoverURL}}" alt=""></div>{{else if .CoverClass}}<div class="cover {{.CoverClass}}"></div>{{end}}
<main>{{if .Updated}}<p class="fresh">Updated {{.Updated}} · <a href="{{.Self}}">Reload</a> for the latest</p>{{end}}<article>{{if .Icon}}<div class="icon">{{.Icon}}</div>{{end}}<h1 class="title">{{.Title}}</h1>
{{.Body}}
</article>{{if .Duplicate}}<footer><a href="{{.Duplicate}}">Copy to my workspace</a></footer>{{end}}</main></body></html>{{end}}
{{define "notice"}}{{template "head" .}}
<body><main class="notice"><div class="ncard">{{if .Mark}}<div class="mark" aria-hidden="true">{{.Mark}}</div>{{end}}<h1>{{.Heading}}</h1><p>{{.Line}}</p>{{if .Action}}<p><a class="action" href="{{.ActionURL}}">{{.Action}}</a></p>{{end}}</div><p class="made"><span>Made with OneCamp</span></p></main></body></html>{{end}}
`))

type docData struct {
	Title, Canonical, Updated, Duplicate  string
	Self                                  string
	Indexable                             bool
	Style                                 template.CSS
	Body                                  template.HTML
	BodyClass, Icon, CoverURL, CoverClass string
	Heading, Line, Action, ActionURL      string
	// Mark is a notice's drawing, in ink on the neutral tile (marks).
	Mark template.HTML
}

func render(name string, d docData) []byte {
	d.Style = template.CSS(style)
	var b bytes.Buffer
	if err := documents.ExecuteTemplate(&b, name, d); err != nil {
		return []byte("<!doctype html><title>Unavailable</title><p>This page couldn't be shown.</p>")
	}
	return b.Bytes()
}

// bodyClass is the page's look as its body's classes. Pure.
func bodyClass(p *Page) string {
	out := ""
	add := func(c string) {
		if out != "" {
			out += " "
		}
		out += c
	}
	switch p.Font {
	case "serif", "mono":
		add(p.Font)
	}
	if p.SmallText {
		add("small")
	}
	if p.FullWidth {
		add("wide")
	}
	return out
}

// ago is how long before now a page was updated, as its freshness line says
// it: "just now", "5 minutes ago", "yesterday", "on 2 October 2026". Pure.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	plural := func(n int, one string) string {
		if n == 1 {
			return "1 " + one + " ago"
		}
		return strconv.Itoa(n) + " " + one + "s ago"
	}
	switch {
	case t.IsZero():
		return ""
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour")
	case d < 48*time.Hour:
		return "yesterday"
	case d < 30*24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	}
	return "on " + t.UTC().Format("2 January 2006")
}

// PageDocument is a page: a snapshot of the doc, which says how fresh it is
// and links to itself (self) to reload it. canonical is its own address
// (none for a link's token); duplicate, when its owner allows copies, the
// web app's address that copies it. now is the moment it is answered.
func PageDocument(p *Page, canonical, duplicate, self string, now time.Time) Document {
	d := docData{
		Title: p.Title, Canonical: canonical, Updated: ago(p.UpdatedAt, now), Self: self, Indexable: p.Indexable,
		Body:      template.HTML(p.HTML), // made fit by renderer.body: walked and allowlisted
		Duplicate: duplicate, BodyClass: bodyClass(p), Icon: p.Icon, CoverURL: p.CoverURL,
	}
	if d.CoverURL == "" && galleryCovers[p.Cover] {
		d.CoverClass = "cover-" + p.Cover
	}
	body := render("page", d)
	sum := sha256.Sum256(body)
	return Document{Status: http.StatusOK, Body: body, ETag: `"` + hex.EncodeToString(sum[:12]) + `"`, Indexable: p.Indexable,
		Revalidate: true, Private: p.Token != ""}
}

// marks are the notices' drawings: plain strokes in the text's own colour, no
// emoji (they drew in each system's own style, off the page's look).
var marks = map[string]template.HTML{
	"lock":    `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/></svg>`,
	"compass": `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M15.5 8.5 13.4 13.4 8.5 15.5 10.6 10.6z"/></svg>`,
	"clock":   `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>`,
	"alert":   `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 8v5"/><path d="M12 16h.01"/></svg>`,
}

// LockedDocument is a password page asked for without its token: the way in
// is the web app's, since this page can't hold a form (form-action 'none').
func LockedDocument(unlockURL string) Document {
	return Document{Status: http.StatusUnauthorized, Body: render("notice", docData{
		Title: "This page needs a password", Heading: "This page needs a password", Mark: marks["lock"],
		Line: "Whoever shared it with you has its password.", Action: "Enter the password", ActionURL: unlockURL,
	})}
}

// MissingDocument is an address with no page: never published, unpublished,
// or not to be shown. It says nothing about any doc.
func MissingDocument() Document {
	return Document{Status: http.StatusNotFound, Body: render("notice", docData{
		Title: "No page here", Heading: "There's no page here", Mark: marks["compass"],
		Line: "The address may be mistyped, or the page may no longer be published.",
	})}
}

// GoneDocument is a page whose expiry has passed.
func GoneDocument() Document {
	return Document{Status: http.StatusGone, Body: render("notice", docData{
		Title: "This page has expired", Heading: "This page has expired", Mark: marks["clock"],
		Line: "Whoever published it set it to stop being shown, and that time has passed.",
	})}
}

// BusyDocument is a page asked for more often than it answers in a minute.
func BusyDocument() Document {
	return Document{Status: http.StatusTooManyRequests, Body: render("notice", docData{
		Title: "This page is busy", Heading: "This page is busy", Mark: marks["clock"],
		Line: "It has had a lot of visitors in the last minute. Try again in a moment.",
	})}
}

// UnavailableDocument is a page that couldn't be read just now.
func UnavailableDocument() Document {
	return Document{Status: http.StatusServiceUnavailable, Body: render("notice", docData{
		Title: "This page couldn't be shown", Heading: "This page couldn't be shown", Mark: marks["alert"],
		Line: "Something went wrong on our side. Try again in a moment.",
	})}
}

// stamp is a time as the page's footer says it; zero when there is none.
func stamp(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
