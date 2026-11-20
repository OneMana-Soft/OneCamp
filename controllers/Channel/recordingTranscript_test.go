package controllers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	channelDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Channel"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// A channel there isn't answers its recording transcript route exactly as a
// channel the asker isn't in does: the same status, headers and body, with
// nothing of the read's error (V-1163). A read that failed is "try again".
func TestTheChannelTranscriptAnswersAMissingChannelAsOneTheAskerIsNotIn(t *testing.T) {
	keep := channelRecordingTranscript
	t.Cleanup(func() { channelRecordingTranscript = keep })
	ask := func(read func() (*dgraphStruct.DgraphChannel, error)) *httptest.ResponseRecorder {
		channelRecordingTranscript = func(context.Context, uuid.UUID, string, string, int, int) (*dgraphStruct.DgraphChannel, error) {
			return read()
		}
		ch := uuid.NewString()
		route := chi.NewRouteContext()
		route.URLParams.Add("channel_uuid", ch)
		route.URLParams.Add("egress_id", "EG_1")
		ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, userModels.UserInfo{})
		w := httptest.NewRecorder()
		GetChannelRecordingTranscript(w, httptest.NewRequest(http.MethodGet, "/channel/getRecordingTranscript/"+ch+"/EG_1?pageIndex=0&pageSize=20", nil).
			WithContext(context.WithValue(ctx, chi.RouteCtxKey, route)))
		return w
	}

	notIn := ask(func() (*dgraphStruct.DgraphChannel, error) { return &dgraphStruct.DgraphChannel{IsMember: 0}, nil })
	missing := ask(func() (*dgraphStruct.DgraphChannel, error) { return nil, channelDgraphModels.ErrNotFound })
	if missing.Code != notIn.Code || !reflect.DeepEqual(missing.Header(), notIn.Header()) || missing.Body.String() != notIn.Body.String() {
		t.Errorf("a channel there isn't: %d %v %s, want what one the asker isn't in gets: %d %v %s",
			missing.Code, missing.Header(), missing.Body.String(), notIn.Code, notIn.Header(), notIn.Body.String())
	}
	failed := ask(func() (*dgraphStruct.DgraphChannel, error) { return nil, errors.New("dgraph at 10.0.0.9 refused") })
	if body := failed.Body.String(); failed.Code != http.StatusServiceUnavailable || !strings.Contains(body, "Try again") || strings.Contains(body, "10.0.0.9") {
		t.Errorf("a read that failed: %d %s, want 503 try again, and nothing of the error", failed.Code, body)
	}
}
