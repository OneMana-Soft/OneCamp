package helpers

import (
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
