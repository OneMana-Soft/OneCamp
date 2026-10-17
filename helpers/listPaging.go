package helpers

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
)

// ErrListPaging is a list request that says neither which page it wants nor
// that it wants everything. Answered 400 with this text: it once came back as
// "Noi Authorised", and as a 401 that made the web app refresh its session.
var ErrListPaging = errors.New("say which page to list (pageSize and pageIndex), or ask for everything with getAll=true")

// ListPaging reads a list request's getAll / pageSize / pageIndex parameters.
func ListPaging(q url.Values) (getAll bool, pageSize, pageIndex int, err error) {
	getAll = q.Get("getAll") != ""
	sizeStr, indexStr := q.Get("pageSize"), q.Get("pageIndex")
	if !getAll && (sizeStr == "" || indexStr == "") {
		return false, 0, 0, ErrListPaging
	}
	if sizeStr != "" {
		if pageSize, err = strconv.Atoi(sizeStr); err != nil || pageSize < 0 {
			return false, 0, 0, errors.New("pageSize must be a whole number")
		}
	}
	if indexStr != "" {
		if pageIndex, err = strconv.Atoi(indexStr); err != nil || pageIndex < 0 {
			return false, 0, 0, errors.New("pageIndex must be a whole number")
		}
	}
	return getAll, pageSize, pageIndex, nil
}

// boardClosedDefault, boardClosedMax, boardOpenDefault and boardOpenMax
// mirror models/dgraph's BoardClosedLimit, BoardClosedMax, BoardOpenLimit and
// BoardOpenMax (helpers cannot import models).
const (
	boardClosedDefault = 200
	boardClosedMax     = 2000
	boardOpenDefault   = 1000
	boardOpenMax       = 5000
)

// ClosedLimit reads a board request's closedLimit: how many of its newest
// done and cancelled tasks to load. Missing or bad means the default; it is
// held to at least the default and at most the maximum.
func ClosedLimit(q url.Values) int {
	n, err := strconv.Atoi(q.Get("closedLimit"))
	if err != nil || n < boardClosedDefault {
		return boardClosedDefault
	}
	if n > boardClosedMax {
		return boardClosedMax
	}
	return n
}

// OffsetCursor is the cursor for the page of a list that starts at offset: an
// opaque string a list hands out as its next page's, and the app sends back as
// it is. CursorOffset reads it.
func OffsetCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// CursorOffset is where a cursor OffsetCursor made says the next page starts:
// 0 for no cursor, and for anything that isn't one.
func CursorOffset(cursor string) int {
	n, _ := ReadCursor(cursor)
	return n
}

// ReadCursor is CursorOffset for a list that refuses a cursor it didn't hand
// out, as the public API's do: ok is false for anything OffsetCursor can't
// have made. No cursor is the first page.
func ReadCursor(cursor string) (offset int, ok bool) {
	if cursor == "" {
		return 0, true
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(string(b))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// LimitParam reads a list request's limit: def when it is missing, not a
// whole number, or below one, and never more than max.
func LimitParam(q url.Values, def, max int) int {
	return boundedParam(q, "limit", def, max)
}

// OpenLimit reads a board request's openLimit, by LimitParam's rule: how many
// cards of each open column to send, first in board order.
func OpenLimit(q url.Values) int {
	return boundedParam(q, "openLimit", boardOpenDefault, boardOpenMax)
}

// boundedParam reads the whole number a request holds under key: def when it
// is missing, not a whole number, or below one, and never more than max.
func boundedParam(q url.Values, key string, def, max int) int {
	n, err := strconv.Atoi(q.Get(key))
	if err != nil || n < 1 {
		return def
	}
	return min(n, max)
}
