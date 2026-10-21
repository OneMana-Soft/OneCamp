package business

import (
	"strings"
	"testing"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
)

// Every built-in embed of the web app (lib/embeds/providers.ts, its test's
// CASES) is a card naming its site; the hosts here are byHost's.
func TestTheCardsNameTheWebsSites(t *testing.T) {
	for href, want := range map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=1m30s":                             "YouTube",
		"https://youtu.be/dQw4w9WgXcQ?t=42":                                               "YouTube",
		"https://vimeo.com/76979871":                                                      "Vimeo",
		"https://www.loom.com/share/0281766fa2d04bb788eaf19e65135184":                     "Loom",
		"https://www.figma.com/design/AbC123/Launch-plan?node-id=1-2":                     "Figma",
		"https://docs.google.com/document/d/1AbCdEf_gH/edit#heading=h.1":                  "Google Docs",
		"https://docs.google.com/spreadsheets/d/1AbCdEf_gH/edit#gid=0":                    "Google Sheets",
		"https://docs.google.com/presentation/d/1AbCdEf_gH/edit":                          "Google Slides",
		"https://www.google.com/maps/place/Lalbagh+Botanical+Garden/@12.9507,77.5848,15z": "Google Maps",
		"https://maps.google.com/maps?q=Bengaluru":                                        "Google Maps",
		"https://gist.github.com/octocat/6cad326836d38bd3a7ae":                            "GitHub Gist",
		"https://codepen.io/team/pen/abcDEF":                                              "CodePen",
		"https://excalidraw.com/#json=abc123,def456":                                      "Excalidraw",
		"https://miro.com/app/board/uXjVabc=/":                                            "Miro",
		"https://open.spotify.com/intl-de/track/4uLU6hMCjMI75M1A2tKUQC":                   "Spotify",
		"https://x.com/jack/status/20":                                                    "X",
		"https://twitter.com/jack/status/20":                                              "X",
	} {
		if name, _, ok := embedSite(href, nil); !ok || name != want {
			t.Errorf("%s: %q %v, want %q", href, name, ok, want)
		}
	}
}

// Anything else stays a link: another site, a Google search, a link with a
// password in it, the workspace's own addresses; an admin's host only over
// https, and covered as the web covers it.
func TestOnlyTheWorkspacesSitesBecomeCards(t *testing.T) {
	hosts := []string{"embed.example.com", "*.charts.example.org"}
	for _, href := range []string{
		"https://example.com/watch?v=dQw4w9WgXcQ",
		"https://www.google.com/search?q=maps",
		"https://user:pw@www.youtube.com/watch?v=dQw4w9WgXcQ",
		"javascript:alert(1)",
		"https://app.example.test/app/doc/1",
		"http://embed.example.com/v/1",
		"https://charts.example.org/x",
		"https://other.example.com/",
	} {
		if _, _, ok := embedSite(href, hosts); ok {
			t.Errorf("%s became a card", href)
		}
	}
	for href, host := range map[string]string{
		"https://embed.example.com/v/1":   "embed.example.com",
		"https://eu.charts.example.org/x": "eu.charts.example.org",
		"https://Embed.Example.com./v/2":  "embed.example.com",
	} {
		if name, got, ok := embedSite(href, hosts); !ok || got != host || name != host {
			t.Errorf("%s: %q %q %v", href, name, got, ok)
		}
	}
	if !hostAllowed("Embed.Example.com", []string{" embed.example.com. "}) || hostAllowed("evil-example.com", []string{"*.example.com"}) || hostAllowed("example.com", []string{""}) {
		t.Error("an admin's entries are read as the web reads them")
	}
}

// On the page an allowed embed is a card with a plain link, never a frame;
// any other embed is a link; a copy keeps its link as before (D-26).
func TestAnEmbedIsACardNeverAFrame(t *testing.T) {
	saved := `<div data-type="web-embed" data-src="https://www.youtube.com/watch?v=dQw4w9WgXcQ"></div>
<div data-type="web-embed" data-src="https://embed.example.com/v/1"></div>
<div data-type="web-embed" data-src="https://example.com/not-allowed"></div>`
	out := renderer{docUUID: docID, fileURL: fileURL(Ref{Slug: "launch-notes"}, ""), embedHosts: []string{"embed.example.com"}}.body(saved)
	for _, want := range []string{
		`<div class="embed"><span class="es">YouTube</span><span class="eh">youtube.com</span><a href="https://www.youtube.com/watch?v=dQw4w9WgXcQ" rel="nofollow noreferrer">Open on YouTube</a></div>`,
		`<span class="es">embed.example.com</span>`,
		`<a href="https://example.com/not-allowed" rel="nofollow noreferrer">https://example.com/not-allowed</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the page lacks %s:\n%s", want, out)
		}
	}
	for _, never := range []string{"<iframe", "target=", "data-src"} {
		if strings.Contains(out, never) {
			t.Errorf("the page has %q:\n%s", never, out)
		}
	}
	if n := strings.Count(out, `class="embed"`); n != 2 {
		t.Errorf("%d cards, want 2", n)
	}
}

// A kept page is drawn again when the admin's embed hosts change.
func TestAKeptPageFollowsTheEmbedList(t *testing.T) {
	g := &guestModel.GuestGrant{ResourceID: docID}
	c := cachedOf(g, &Page{}, []string{"embed.example.com"})
	if !c.fits(g, []string{"embed.example.com"}) {
		t.Fatal("the same list fits")
	}
	if c.fits(g, nil) || c.fits(g, []string{"embed.example.com", "other.example.com"}) {
		t.Fatal("a changed list doesn't")
	}
}
