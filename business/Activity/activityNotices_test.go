package business

import (
	"slices"
	"testing"
	"time"

	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
)

func itemAt(kind string, minute int) dgraphModels.UnifiedActivityItem {
	t := time.Date(2026, 10, 12, 10, minute, 0, 0, time.UTC)
	return dgraphModels.UnifiedActivityItem{ActivityType: kind, Time: t.Format(time.RFC3339Nano)}
}

func shape(items []dgraphModels.UnifiedActivityItem) []string {
	var out []string
	for _, i := range items {
		out = append(out, i.ActivityType+i.Time[14:16])
	}
	return out
}

// Notices merge into a page by time; a source that had more can only be merged
// down to its oldest item given, so nothing it held back is skipped by the
// next page's cursor.
func TestNoticesMergeIntoAPageOfTheFeedByTime(t *testing.T) {
	page := dgraphModels.UnifiedActivityPagination{Activities: []dgraphModels.UnifiedActivityItem{
		itemAt("MENTION", 50), itemAt("COMMENT", 40), itemAt("MENTION", 30),
	}}
	notices := []dgraphModels.UnifiedActivityItem{itemAt("NOTICE", 45), itemAt("NOTICE", 35)}

	got := mergeNotices(page, notices, 4)
	if want := []string{"MENTION50", "NOTICE45", "COMMENT40", "NOTICE35"}; !slices.Equal(shape(got.Activities), want) || !got.HasMore {
		t.Errorf("merged %v (more %v), want %v and more", shape(got.Activities), got.HasMore, want)
	}

	// The graph had more than it gave, older than :30: a notice from :20 waits
	// for the next page, which reads the graph from :30 down too.
	page.HasMore = true
	got = mergeNotices(page, []dgraphModels.UnifiedActivityItem{itemAt("NOTICE", 20)}, 10)
	if want := []string{"MENTION50", "COMMENT40", "MENTION30"}; !slices.Equal(shape(got.Activities), want) || !got.HasMore {
		t.Errorf("merged %v (more %v), want %v and more", shape(got.Activities), got.HasMore, want)
	}

	// The notices had more than they gave (limit+1 read): the graph's older
	// items wait below the last notice given.
	page.HasMore = false
	got = mergeNotices(page, []dgraphModels.UnifiedActivityItem{itemAt("NOTICE", 55), itemAt("NOTICE", 45), itemAt("NOTICE", 35)}, 2)
	if want := []string{"NOTICE55", "MENTION50"}; !slices.Equal(shape(got.Activities), want) || !got.HasMore {
		t.Errorf("merged %v (more %v), want %v and more", shape(got.Activities), got.HasMore, want)
	}

	// No notices: the page as it was.
	if got := mergeNotices(page, nil, 10); !slices.Equal(shape(got.Activities), shape(page.Activities)) {
		t.Errorf("with no notices the page changed: %v", shape(got.Activities))
	}
}
