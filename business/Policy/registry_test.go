package business

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

type store struct {
	values  map[string]string
	records []string
	failRec bool
}

func withStore(t *testing.T, s *store) *Registry {
	t.Helper()
	oldR, oldW, oldRec, oldNow := readValues, writeValue, recordIt, now
	t.Cleanup(func() { readValues, writeValue, recordIt, now = oldR, oldW, oldRec, oldNow })
	readValues = func([]string) (map[string]string, error) {
		out := map[string]string{}
		for k, v := range s.values {
			out[k] = v
		}
		return out, nil
	}
	writeValue = func(k, v string) error { s.values[k] = v; return nil }
	recordIt = func(_ context.Context, _ *uuid.UUID, _, kind, action, category, summary string, _ map[string]interface{}) error {
		if s.failRec {
			return errors.New("audit down")
		}
		s.records = append(s.records, kind+"|"+action+"|"+category+"|"+summary)
		return nil
	}
	return &Registry{}
}

var admin = Changer{UserID: uuid.New(), Email: "admin@example.test"}

// A change is checked, stored, recorded, and read back at once.
func TestAPolicyChangeIsCheckedStoredAndRecorded(t *testing.T) {
	s := &store{values: map[string]string{}}
	r := withStore(t, s)
	ctx := context.Background()
	if !r.Bool(ctx, DocsExportMembers) {
		t.Fatal("unset, members export, as before policies")
	}
	in, err := r.Set(ctx, DocsExportMembers.Key, json.RawMessage(`false`), admin)
	if err != nil || in.Value != false || !in.IsSet {
		t.Fatalf("set: %+v %v", in, err)
	}
	if r.Bool(ctx, DocsExportMembers) {
		t.Error("the change wasn't read back")
	}
	if len(s.records) != 1 || s.records[0] != "human|policy.changed|settings|Changed “Members can export docs” from on to off" {
		t.Errorf("recorded %v", s.records)
	}
	for key, value := range map[string]string{
		DocsExportMembers.Key: `"no"`, GuestLinkMaxDays.Key: `-1`, EmbedAllowlist.Key: `["javascript:alert(1)"]`, "docs.unknown": `true`,
	} {
		if _, err := r.Set(ctx, key, json.RawMessage(value), admin); err == nil {
			t.Errorf("%s took %s", key, value)
		}
	}
	if _, err := r.Set(ctx, EmbedAllowlist.Key, json.RawMessage(`[" YouTube.com ", "*.figma.com", "youtube.com"]`), admin); err != nil {
		t.Fatal(err)
	}
	if got := r.List(ctx, EmbedAllowlist); strings.Join(got, ",") != "youtube.com,*.figma.com" {
		t.Errorf("the list reads %v", got)
	}
	if _, err := r.Set(ctx, EmbedAllowlist.Key, json.RawMessage(`null`), admin); err != nil || r.List(ctx, EmbedAllowlist) != nil {
		t.Errorf("null is no list: %v %v", r.List(ctx, EmbedAllowlist), err)
	}
	if _, err := r.Set(ctx, GuestLinkMaxDays.Key, json.RawMessage(`30`), admin); err != nil || r.Int(ctx, GuestLinkMaxDays) != 30 {
		t.Errorf("guest links: %d %v", r.Int(ctx, GuestLinkMaxDays), err)
	}
}

// A change the audit log can't record is undone and refused: no policy
// changes unrecorded.
func TestAnUnrecordedChangeIsUndone(t *testing.T) {
	s := &store{values: map[string]string{DocsPublishMembers.Key: "true"}, failRec: true}
	r := withStore(t, s)
	if _, err := r.Set(context.Background(), DocsPublishMembers.Key, json.RawMessage(`false`), admin); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("err %v", err)
	}
	if s.values[DocsPublishMembers.Key] != "true" || !r.Bool(context.Background(), DocsPublishMembers) {
		t.Error("the unrecorded change stood")
	}
	// Never set before: undoing writes the default back.
	if _, err := r.Set(context.Background(), GuestLinkMaxDays.Key, json.RawMessage(`7`), admin); !errors.Is(err, ErrNotRecorded) || s.values[GuestLinkMaxDays.Key] != "0" {
		t.Errorf("undone to %q, err %v", s.values[GuestLinkMaxDays.Key], err)
	}
}

// A route behind a policy that's off refuses members, lets admins through,
// and follows a change within the cache's time.
func TestARoutesPolicyIsAskedAtEachRequest(t *testing.T) {
	s := &store{values: map[string]string{DocsExportMembers.Key: "false"}}
	r := withStore(t, s)
	clock := time.Now()
	now = func() time.Time { return clock }
	h := r.Require(DocsExportMembers)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	ask := func(adminUser bool) int {
		var u userModels.UserInfo
		u.UserPostgresInfo.IsAdmin = adminUser
		req := httptest.NewRequest(http.MethodPost, "/doc/export", nil)
		req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, u))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if ask(false) != http.StatusForbidden || ask(true) != http.StatusTeapot {
		t.Error("off: a member wasn't refused, or an admin was")
	}
	s.values[DocsExportMembers.Key] = "true" // another instance changed it
	clock = clock.Add(cacheFor + time.Second)
	if ask(false) != http.StatusTeapot {
		t.Error("the change wasn't seen once the cache had expired")
	}
}

// Every policy is listed for an admin, with its value and default.
func TestEveryPolicyIsListed(t *testing.T) {
	r := withStore(t, &store{values: map[string]string{GuestLinkMaxDays.Key: "14"}})
	all := r.ListAll(context.Background())
	if len(all) != 4 {
		t.Fatalf("listed %d", len(all))
	}
	for _, in := range all {
		if in.Key == GuestLinkMaxDays.Key && (in.Value != 14 || !in.IsSet || *in.Max != 3650) {
			t.Errorf("guest links: %+v", in)
		}
		if in.Key == DocsExportMembers.Key && (in.Value != true || in.IsSet) {
			t.Errorf("export: %+v", in)
		}
	}
}
