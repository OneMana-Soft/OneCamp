package business

import (
	"strings"
	"unicode"
	"unicode/utf8"

	query "github.com/akashc777/OneCamp/business/QuerySpec"
)

// Refusal is a query's refusal in its own words, for the person whose view or
// call asked: "Grouped this way, the rows fall into more than 1,000 groups".
// The kind it's of ("row query: that can't be asked of this table yet: "), and
// what wrapped it on the way out, are for the logs (CR-62): the app's notice
// (controllers/DataTable WriteQueryErr) and the API's error body
// (business/DataTable apiErr) both say this.
func Refusal(err error) string {
	msg := err.Error()
	for _, kind := range []error{ErrCantQuery, query.ErrBadSpec} {
		if i := strings.Index(msg, kind.Error()+": "); i >= 0 {
			msg = msg[i+len(kind.Error())+2:]
		}
	}
	msg = strings.TrimPrefix(strings.TrimPrefix(msg, "row query: "), "query: ")
	r, n := utf8.DecodeRuneInString(msg)
	if n == 0 {
		return msg
	}
	return string(unicode.ToUpper(r)) + msg[n:]
}
