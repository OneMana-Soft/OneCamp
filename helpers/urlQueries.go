package helpers

import (
	"regexp"
	"strings"
)

// A provider's request can carry its credentials in the URL's query: Trello
// takes key= and token= there, and a presigned object-store link its
// signature. net/http puts the whole URL in the error it returns
// (`Get "https://api.trello.com/1/boards?key=…&token=…": dial tcp: …`), and an
// import stored that text as the job's error, showed it to the admin and
// broadcast it to every admin's open page.
var urlWithQuery = regexp.MustCompile(`(?i)(\b[a-z][a-z0-9+.-]*://[^\s"'<>?#]*)[?#][^\s"'<>]*`)

// WithoutURLQueries is s with the query string (and fragment) taken off every
// URL in it, so error text can be stored, shown and broadcast without what a
// provider put there. The rest of each URL stays, so the error still says
// which call failed. Pure.
func WithoutURLQueries(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	return urlWithQuery.ReplaceAllString(s, "$1")
}
