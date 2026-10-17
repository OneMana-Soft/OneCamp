package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	business "github.com/akashc777/OneCamp/business/Activity"
	"github.com/akashc777/OneCamp/helpers"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// The feed reads at most a page, whatever the app asks for. Its limit was the
// client's, uncapped, and it drives the notices too (limit+1 of them), each a
// Postgres read, an access check and a title read: ?limit=1000000 asked for a
// million. A limit that isn't a page size (none, junk, below one) is the
// default; one below zero also cut the feed at [:-5], which panics.
func TestTheFeedReadsAtMostAPage(t *testing.T) {
	prevFeed, prevNotices := unifiedActivity, business.NoticeSource
	t.Cleanup(func() { unifiedActivity, business.NoticeSource = prevFeed, prevNotices })
	var feedAsked, noticesAsked int
	unifiedActivity = func(_ context.Context, _ uuid.UUID, _ string, _ time.Time, limit int) (dgraphModels.UnifiedActivityPagination, error) {
		feedAsked = limit
		return dgraphModels.UnifiedActivityPagination{}, nil
	}
	business.NoticeSource = func(_ context.Context, _ string, _ time.Time, limit int) ([]dgraphModels.UnifiedActivityItem, error) {
		noticesAsked = limit
		return nil, nil
	}
	for _, c := range []struct {
		limit string
		want  int
	}{
		{"1000000", maxActivityPage},
		{"51", maxActivityPage},
		{"50", 50},
		{"10", 10},
		{"", activityPageSize},
		{"twenty", activityPageSize},
		{"0", activityPageSize},
		{"-5", activityPageSize},
	} {
		feedAsked, noticesAsked = 0, 0
		req := httptest.NewRequest(http.MethodGet, "/activity/unified?notices=1&limit="+c.limit, nil).
			WithContext(context.WithValue(context.Background(), helpers.UserInfoContextKey, userModels.UserInfo{}))
		rec := httptest.NewRecorder()
		GetUnifiedActivity(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("limit %q answered %d %s", c.limit, rec.Code, rec.Body.String())
		}
		if feedAsked != c.want || noticesAsked != c.want+1 {
			t.Errorf("limit %q read %d of the feed and asked for %d notices; want %d and %d", c.limit, feedAsked, noticesAsked, c.want, c.want+1)
		}
	}
}
