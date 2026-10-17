package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	business "github.com/akashc777/OneCamp/business/Channel"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Who can post in a channel answers what happened: a rule broken is the
// request's fault (400), a channel that isn't there a 404, and a store that
// didn't answer, reading or writing, a 503 the app offers Try again for. A
// write that failed answered 400 (8c596697 changed only its words).
func TestSetChannelPostPolicyAnswersWhatHappened(t *testing.T) {
	prevRead, prevWrite := readPostPolicyChannel, setChannelPostPolicy
	t.Cleanup(func() { readPostPolicyChannel, setChannelPostPolicy = prevRead, prevWrite })
	admin := &dgraphStruct.DgraphChannel{Uuid: uuid.NewString(), IsAdmin: 1}
	cases := []struct {
		name     string
		read     error
		member   bool
		write    error
		want     int
		wantWord string
	}{
		{name: "saved", want: http.StatusOK},
		{name: "a policy there is no such thing as", write: business.ErrInvalidPostPolicy, want: http.StatusBadRequest, wantWord: "Choose who can post"},
		{name: "the store didn't take it", write: errors.New("postgres: connection refused"), want: http.StatusServiceUnavailable, wantWord: "Try again in a moment"},
		{name: "a channel that isn't there", read: helpers.ErrNotFound, want: http.StatusNotFound},
		{name: "a read that failed", read: errors.New("dgraph: deadline exceeded"), want: http.StatusServiceUnavailable},
		{name: "someone who isn't its admin", member: true, want: http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			readPostPolicyChannel = func(context.Context, uuid.UUID, string) (*dgraphStruct.DgraphChannel, error) {
				if c.read != nil {
					return nil, c.read
				}
				if c.member {
					return &dgraphStruct.DgraphChannel{Uuid: admin.Uuid}, nil
				}
				return admin, nil
			}
			setChannelPostPolicy = func(context.Context, uuid.UUID, string) error { return c.write }
			raw, _ := json.Marshal(map[string]string{"channel_uuid": admin.Uuid, "post_policy": "admins_only"})
			ctx := context.WithValue(context.Background(), helpers.UserInfoContextKey, userModels.UserInfo{})
			rec := httptest.NewRecorder()
			SetChannelPostPolicy(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(ctx))
			if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.wantWord) {
				t.Errorf("answered %d %s, want %d saying %q", rec.Code, rec.Body.String(), c.want, c.wantWord)
			}
		})
	}
}
