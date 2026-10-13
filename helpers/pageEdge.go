package helpers

// Where a page of a conversation starts.
//
// Pages used to be asked for in whole seconds: an older page from the floor
// of the oldest message's time, a newer one from the ceiling of the newest's,
// and the server compared strictly against its own times, to the nanosecond.
// A message that shared a second with a page's edge was in neither page, and
// nothing said so: a bot's burst, send-laters firing together, the Slack
// bridge.
//
// So an app names the message at the page's edge: ?at= its time as the
// server wrote it, and ?id= its id. The page is read from that instant
// inclusive, so a message at the same instant isn't skipped, and the edge
// itself is left out by its id. An app from before sends whole seconds, and
// is answered as it was.

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// PageEdge is the message a page starts next to: its exact time, and its id
// ("" when the edge came as whole seconds, from an older app).
type PageEdge struct {
	At time.Time
	ID string
}

// PageEdgeOf reads a page's edge from a request: ?at= (RFC 3339, to the
// nanosecond) and ?id= (a uuid) when both are there, else the whole seconds
// from the path.
func PageEdgeOf(r *http.Request, seconds string) (PageEdge, error) {
	q := r.URL.Query()
	if at, id := q.Get("at"), q.Get("id"); at != "" || id != "" {
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return PageEdge{}, errors.New("at must be a message's time, RFC 3339")
		}
		if parsed, err := uuid.Parse(id); err != nil || parsed.String() != id {
			return PageEdge{}, errors.New("id must be a message's id")
		}
		return PageEdge{At: t, ID: id}, nil
	}
	n, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return PageEdge{}, errors.New("time_stamp must be a whole number of seconds")
	}
	return PageEdge{At: time.Unix(n, 0)}, nil
}

// Before is the time an older page is read strictly before: just past the
// edge's own instant when the edge names its message, so a message at that
// instant is read too.
func (e PageEdge) Before() time.Time {
	if e.ID != "" {
		return e.At.Add(time.Nanosecond)
	}
	return e.At
}

// After is the time a newer page is read strictly after, the same way.
func (e PageEdge) After() time.Time {
	if e.ID != "" {
		return e.At.Add(-time.Nanosecond)
	}
	return e.At
}

// PageFrom is a page read from an edge, at most count long: the edge's own
// message left out, and whether there may be more past it. read is what the
// store answered, up to count+1 messages.
func PageFrom[T any](read []T, idOf func(T) string, edge PageEdge, count int) (page []T, hasMore bool) {
	page = read
	if edge.ID != "" {
		page = make([]T, 0, len(read))
		for _, m := range read {
			if idOf(m) != edge.ID {
				page = append(page, m)
			}
		}
	}
	if len(page) > count {
		return page[:count], true
	}
	return page, len(read) > count
}
