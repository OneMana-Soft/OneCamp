package domain

import (
	"net/url"
	"strings"
)

// LinkFamily is what a link's provider's document is, for its mark (docs F1b):
// doc, sheet, slides, drive, design, code, video, or site for any other. It is
// the web's linkProvider family (lib/docs/docNode.ts), and testdata/linkfamily
// holds the two to the same answers. The work graph's chip names a link by its
// family alone, never by its address (DGRAPH-LEAD-5), so the panel draws it as
// the docs tree does.
func LinkFamily(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return "site"
	}
	host := strings.ToLower(u.Hostname())
	within := func(site string) bool { return host == site || strings.HasSuffix(host, "."+site) }
	switch {
	case host == "docs.google.com":
		switch {
		case strings.HasPrefix(u.Path, "/document/"), strings.HasPrefix(u.Path, "/forms/"):
			return "doc"
		case strings.HasPrefix(u.Path, "/spreadsheets/"):
			return "sheet"
		case strings.HasPrefix(u.Path, "/presentation/"):
			return "slides"
		}
		return "drive"
	case within("sharepoint.com"):
		switch {
		case strings.Contains(u.Path, "/:w:/"):
			return "doc"
		case strings.Contains(u.Path, "/:x:/"):
			return "sheet"
		case strings.Contains(u.Path, "/:p:/"):
			return "slides"
		}
		return "drive"
	case host == "drive.google.com", host == "onedrive.live.com", host == "1drv.ms", within("dropbox.com"), within("box.com"):
		return "drive"
	case within("figma.com"), within("miro.com"), within("canva.com"):
		return "design"
	case host == "github.com", host == "gist.github.com", within("gitlab.com"):
		return "code"
	case within("notion.so"), within("notion.site"):
		return "doc"
	case youTube(host), within("loom.com"), within("vimeo.com"):
		return "video"
	}
	return "site"
}

// youTube is YouTube's hosts, with or without www.
func youTube(host string) bool {
	switch strings.TrimPrefix(host, "www.") {
	case "youtube.com", "youtu.be", "m.youtube.com":
		return true
	}
	return false
}
