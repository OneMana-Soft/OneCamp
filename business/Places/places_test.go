package business

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	model "github.com/akashc777/OneCamp/models/postgres/Places"
)

// shelf is a kind whose resolver can open some things, by name.
type shelf map[string]Seen

func (s shelf) Resolve(_ context.Context, _ Person, ids []string) (map[string]Seen, error) {
	out := map[string]Seen{}
	for _, id := range ids {
		if seen, ok := s[id]; ok {
			out[id] = seen
		}
	}
	return out, nil
}

type broken struct{}

func (broken) Resolve(context.Context, Person, []string) (map[string]Seen, error) {
	return nil, errors.New("graph down")
}

// RecentItems is what the person opened lately of one kind, newest first as
// the view log keeps it, each named by its kind's resolver: what they can no
// longer open is left out (miro-46: recent boards). A kind no resolver knows
// is ErrResource; a resolver that can't answer is an error.
func TestRecentItemsNamesWhatThePersonMayStillOpenNewestFirst(t *testing.T) {
	Register("test-shelf", shelf{
		"b2": {Title: "Retro", Href: "/app/board/b2"},
		"b1": {Title: "Roadmap", Href: "/app/board/b1"},
	})
	Register("test-broken", broken{})
	now := time.Date(2026, 11, 5, 22, 0, 0, 0, time.UTC)
	var asked struct {
		who, kind string
		since     time.Time
		limit     int
	}
	old := recentViews
	t.Cleanup(func() { recentViews = old })
	recentViews = func(_ context.Context, who, kind string, since time.Time, _ *model.View, limit int) ([]model.View, error) {
		asked.who, asked.kind, asked.since, asked.limit = who, kind, since, limit
		return []model.View{
			{ResourceUUID: "b2", LastViewedAt: now.Add(-time.Minute)},
			{ResourceUUID: "lost", LastViewedAt: now.Add(-2 * time.Minute)},
			{ResourceUUID: "b1", LastViewedAt: now.Add(-time.Hour)},
		}, nil
	}
	since := now.Add(-90 * 24 * time.Hour)
	got, err := RecentItems(context.Background(), Person{UUID: "me", UID: "0x1"}, "test-shelf", since, 20)
	if err != nil {
		t.Fatal(err)
	}
	b2, b1 := now.Add(-time.Minute), now.Add(-time.Hour)
	want := []Item{
		{Kind: Opened, ResourceKind: "test-shelf", ResourceUUID: "b2", Title: "Retro", Href: "/app/board/b2", ViewedAt: &b2},
		{Kind: Opened, ResourceKind: "test-shelf", ResourceUUID: "b1", Title: "Roadmap", Href: "/app/board/b1", ViewedAt: &b1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recent %+v, want %+v", got, want)
	}
	if asked.who != "me" || asked.kind != "test-shelf" || !asked.since.Equal(since) || asked.limit != 20 {
		t.Errorf("read the view log as %+v", asked)
	}

	if _, err := RecentItems(context.Background(), Person{UUID: "me"}, "nothing-placeable", since, 20); !errors.Is(err, ErrResource) {
		t.Errorf("a kind no resolver knows: %v, want ErrResource", err)
	}
	if _, err := RecentItems(context.Background(), Person{UUID: "me"}, "test-broken", since, 20); err == nil {
		t.Error("a resolver that can't answer: no error")
	}
	recentViews = func(context.Context, string, string, time.Time, *model.View, int) ([]model.View, error) {
		return nil, nil
	}
	if got, err := RecentItems(context.Background(), Person{UUID: "me"}, "test-shelf", since, 20); err != nil || got == nil || len(got) != 0 {
		t.Errorf("nothing opened: %v %v, want an empty list", got, err)
	}
}

// viewLog is a person's view log, newest first, read as resource_views is: a
// page of at most limit views after a view (nil: from the newest). It counts
// its reads and what each read began after.
type viewLog struct {
	views  []model.View
	afters []string
}

func (l *viewLog) read(_ context.Context, _, _ string, _ time.Time, after *model.View, limit int) ([]model.View, error) {
	start := 0
	if after != nil {
		l.afters = append(l.afters, after.ResourceUUID)
		for i, v := range l.views {
			if v.ResourceUUID == after.ResourceUUID && v.LastViewedAt.Equal(after.LastViewedAt) {
				start = i + 1
			}
		}
	} else {
		l.afters = append(l.afters, "")
	}
	end := min(start+limit, len(l.views))
	return append([]model.View(nil), l.views[start:end]...), nil
}

// newViewLog is n views of boards "v1".."vn", v1 the newest.
func newViewLog(n int, now time.Time) *viewLog {
	l := &viewLog{}
	for i := 1; i <= n; i++ {
		l.views = append(l.views, model.View{ResourceUUID: fmt.Sprintf("v%d", i), LastViewedAt: now.Add(-time.Duration(i) * time.Minute)})
	}
	return l
}

// countedShelf is a shelf that counts how often it was asked.
type countedShelf struct {
	shelf
	asked [][]string
}

func (c *countedShelf) Resolve(ctx context.Context, who Person, ids []string) (map[string]Seen, error) {
	c.asked = append(c.asked, append([]string(nil), ids...))
	return c.shelf.Resolve(ctx, who, ids)
}

// V-616: the newest views are of things the person can no longer open, and
// older ones are of things they can. limit asked are limit answered: the view
// log is read on, a page at a time after the last view read, with one Resolve
// a page, rather than answering short.
func TestRecentItemsFillsTheLimitFromOlderViewsPastWhatCantBeOpened(t *testing.T) {
	now := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC)
	log := newViewLog(12, now)
	board := &countedShelf{shelf: shelf{"v4": {Title: "Four"}, "v7": {Title: "Seven"}, "v9": {Title: "Nine"}, "v10": {Title: "Ten"}}}
	Register("test-fill", board)
	old := recentViews
	t.Cleanup(func() { recentViews = old })
	recentViews = log.read

	got, err := RecentItems(context.Background(), Person{UUID: "me", UID: "0x1"}, "test-fill", now.Add(-time.Hour), 3)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, it := range got {
		titles = append(titles, it.Title)
	}
	if !reflect.DeepEqual(titles, []string{"Four", "Seven", "Nine"}) {
		t.Errorf("recent %v, want Four, Seven, Nine: three asked, three answered, newest first", titles)
	}
	if want := []string{"", "v3", "v6"}; !reflect.DeepEqual(log.afters, want) {
		t.Errorf("read the view log after %q, want %q: a page at a time after the last view read", log.afters, want)
	}
	if want := [][]string{{"v1", "v2", "v3"}, {"v4", "v5", "v6"}, {"v7", "v8", "v9"}}; !reflect.DeepEqual(board.asked, want) {
		t.Errorf("resolved %v, want %v: once a page", board.asked, want)
	}
	if len(got) == 0 || got[0].ViewedAt == nil || !got[0].ViewedAt.Equal(now.Add(-4*time.Minute)) {
		t.Errorf("recent %+v, want Four first, viewed at its view's time", got)
	}
}

// A view log of things the person can no longer open is read no further than
// five pages (recentScanPages, pinned here as a number so a change to the
// bound fails this test): the list answers short, empty here, rather than
// reading the whole log.
func TestRecentItemsStopsReadingAtTheBound(t *testing.T) {
	now := time.Date(2026, 11, 6, 13, 0, 0, 0, time.UTC)
	log := newViewLog(1000, now)
	board := &countedShelf{shelf: shelf{"v999": {Title: "Far back"}}}
	Register("test-bound", board)
	old := recentViews
	t.Cleanup(func() { recentViews = old })
	recentViews = log.read

	got, err := RecentItems(context.Background(), Person{UUID: "me"}, "test-bound", now.Add(-24*time.Hour), 4)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("recent %+v, want an empty list", got)
	}
	const pages = 5
	if len(log.afters) != pages || len(board.asked) != pages {
		t.Errorf("read %d pages and resolved %d, want %d of each", len(log.afters), len(board.asked), pages)
	}
}
