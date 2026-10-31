package helpers

import "net/url"

// IsWebURL says whether u is a web address people can open: http or https,
// to a host. For an address already read, such as one resolved against a
// page's own. Pure.
func IsWebURL(u *url.URL) bool {
	return u != nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// WebLink is s read as a web address people can open (IsWebURL). A form's URL
// answer and its page after sending (business/Form), a task's link field
// (business/TaskField), an AI runner's address (business/AI), a link card's
// page (business/Unfurl), a sheet's link column (business/DataTable) and an
// embed (business/Publish) each must be one. The web's twin is webURL in
// lib/links/webURL.ts. Pure.
func WebLink(s string) (*url.URL, bool) {
	u, err := url.Parse(s)
	return u, err == nil && IsWebURL(u)
}
