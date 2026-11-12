// Package pagefill fills a list from a source read a page at a time when a
// filter run after each read leaves rows out: what the person can no longer
// open, say. Reading want rows and then filtering them comes back short (20
// asked, 3 left out: 17) though older rows that pass are there; Fill reads on,
// page after page in the source's order, until it has want rows that pass,
// the source ends, or it has read as many pages as it may.
//
// A leaf: it imports nothing of the app's, so any layer may use it.
package pagefill

import "context"

// Reader reads one page of the source: at most size rows after the cursor
// after (C's zero value is the start), in the source's order, and the cursor
// after the page's last row. A page shorter than size is the source's end.
// A keyset cursor (the last row's sort key, ties broken by something unique)
// keeps the pages right while rows come and go; an offset skips or repeats
// them.
type Reader[R, C any] func(ctx context.Context, after C, size int) (rows []R, next C, err error)

// Keep is the filter, called once for each page with the page's rows in
// order: the ones to return, in the same order, as K (a row may become the
// item it names). Once per page, so a filter answers a page in one read,
// never a row at a time.
type Keep[R, K any] func(ctx context.Context, rows []R) ([]K, error)

// Fill reads pages of size rows with read from the start, hands each page to
// keep, and returns what keep kept, in order: at most want. It stops when it
// has want, when a page comes back short (the source's end), or when it has
// read maxPages pages: the bound on the work one call does, so a source whose
// rows are mostly left out answers short rather than being read whole.
//
// size below 1 reads want rows a page; maxPages below 1 reads one page; want
// below 1 is an empty list. An error from read or keep, or a context ended
// between two pages, is answered as it is, with no rows. The list is never
// nil.
func Fill[R, K, C any](ctx context.Context, read Reader[R, C], keep Keep[R, K], size, want, maxPages int) ([]K, error) {
	out := []K{}
	if want < 1 {
		return out, nil
	}
	if size < 1 {
		size = want
	}
	maxPages = max(maxPages, 1)
	var after C
	for page := 0; page < maxPages; page++ {
		if page > 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		rows, next, err := read(ctx, after, size)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		kept, err := keep(ctx, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, kept...)
		if len(out) >= want {
			return out[:want], nil
		}
		if len(rows) < size {
			break
		}
		after = next
	}
	return out, nil
}
