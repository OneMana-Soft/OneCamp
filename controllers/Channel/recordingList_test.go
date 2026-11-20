package controllers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	business "github.com/akashc777/OneCamp/business/Channel"
	"github.com/akashc777/OneCamp/helpers"
	channelDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Channel"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// The channel recording list's read finds neither a channel there isn't nor
// one the asker isn't in (V-1163), and the route answers both with the one
// missing answer; a read that failed is "try again", with nothing of the
// error in the answer; a page read is the page.
func TestTheChannelRecordingListAnswersAMissingChannelAndAFailedReadInItsOwnWords(t *testing.T) {
	keep := channelRecordingList
	t.Cleanup(func() { channelRecordingList = keep })
	ask := func(err error) (int, string) {
		channelRecordingList = func(context.Context, string, string, string, string, int, int) (business.RecordingPagination, error) {
			return business.RecordingPagination{IsMember: 1}, err
		}
		route := chi.NewRouteContext()
		route.URLParams.Add("channel_uuid", "c1")
		ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, userModels.UserInfo{})
		w := httptest.NewRecorder()
		GetAllChannelRecordingList(w, httptest.NewRequest(http.MethodGet, "/ch/getRecordingList/c1", nil).
			WithContext(context.WithValue(ctx, chi.RouteCtxKey, route)))
		return w.Code, w.Body.String()
	}

	if code, body := ask(channelDgraphModels.ErrNotFound); code != http.StatusNotFound || !strings.Contains(body, `"Channel not found"`) {
		t.Errorf("no channel the asker is in: %d %s, want 404 Channel not found", code, body)
	}
	if code, body := ask(errors.New("dgraph at 10.0.0.9 refused")); code != http.StatusServiceUnavailable ||
		!strings.Contains(body, "Try again") || strings.Contains(body, "10.0.0.9") {
		t.Errorf("a read that failed: %d %s, want 503 try again, and nothing of the error", code, body)
	}
	if code, body := ask(nil); code != http.StatusOK || !strings.Contains(body, `"channel_info"`) {
		t.Errorf("a page read: %d %s, want 200 with the page", code, body)
	}
}
