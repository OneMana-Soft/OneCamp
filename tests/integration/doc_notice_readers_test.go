//go:build integration
// +build integration

package integration_test

// Who hears of a doc through the notices no member's comment sends, against
// real Postgres 12, Dgraph and Redis: a guest's comment through a share link
// (business/Comment NotifyDocCommentFor) and a restore that turned the doc's
// guest links off (business/Archive). Each tells only people the doc's read
// rule lets read it now (business/Doc Readers): the owner it was handed to and
// its editors, never its maker once it was handed on and they were taken off
// it, nor someone whose grant on it has ended, even before the expiry sweep
// has taken the grant away.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDocNoticesReachOnlyItsReaders -v

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	archiveBusiness "github.com/akashc777/OneCamp/business/Archive"
	commentBusiness "github.com/akashc777/OneCamp/business/Comment"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	docController "github.com/akashc777/OneCamp/controllers/Doc"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// postAs calls handler as who with body as its JSON, and answers what it said;
// anything but a 200 fails the test, saying what was being done.
func postAs(t *testing.T, ctx context.Context, who userModels.UserInfo, what string, handler http.HandlerFunc, body any) answer {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, who))
	rec := httptest.NewRecorder()
	handler(rec, req)
	out := answer{code: rec.Code}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
	if out.code != 200 {
		t.Fatalf("%s: %d %v", what, out.code, out.body)
	}
	return out
}

// noticedFrom is who, of people, was sent a desktop notice whose id starts
// with prefix (a restore's id ends in the moment it was told).
func (b *noticeBroker) noticedFrom(prefix string, people map[string]userModels.UserInfo) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var who []string
	for name, p := range people {
		topic := helpers.GetMqttTopicForUserActivity(p.UserDgraphInfo.Uuid)
		for _, m := range b.got {
			var msg struct {
				Type int8                         `json:"type"`
				Data mqttStruct.MqttDesktopNotice `json:"data"`
			}
			if m.topic == topic && json.Unmarshal(m.payload, &msg) == nil &&
				msg.Type == mqttStruct.MESSAGE_DESKTOP_NOTICE && strings.HasPrefix(msg.Data.NoticeID, prefix) {
				who = append(who, name)
				break
			}
		}
	}
	slices.Sort(who)
	return who
}

func TestDocNoticesReachOnlyItsReaders(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.SetupDgraph(t)
	broker := &noticeBroker{}
	integration.UseMqttClient(t, broker)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[],"updated":0,"failures":[],"result":"updated","_id":"x"}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	settingsBusiness.ForgetSettingsForTest()
	t.Cleanup(settingsBusiness.ForgetSettingsForTest)
	t.Setenv("RESEND_API_KEY", "re_test_never_sent")

	// Olu made the doc and handed it to Bea, who took Olu off it. Ed edits it.
	// Zed could edit it until a moment that has passed, and the expiry sweep
	// hasn't run since. Sam restores it from the archive's list.
	people := map[string]userModels.UserInfo{}
	for _, name := range []string{"olu", "bea", "ed", "zed", "sam"} {
		id, _, err := userBusiness.CreateUserWithMethod(ctx, name+"@example.test", strings.ToUpper(name[:1])+name[1:], nil, userModels.AuthMethodEmail, false)
		if err != nil {
			t.Fatalf("make %s: %v", name, err)
		}
		pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
		if err != nil || pg == nil {
			t.Fatalf("load %s: %v", name, err)
		}
		node, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
		if err != nil || node == nil {
			t.Fatalf("load %s's node: %v", name, err)
		}
		people[name] = userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *node}
		if _, err := env.PG.Exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2) ON CONFLICT (user_id) DO NOTHING`, id, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	uuidOf := func(name string) string { return people[name].UserDgraphInfo.Uuid }

	doc := str(postAs(t, ctx, people["olu"], "olu makes the doc", docController.CreateDocInTree,
		map[string]any{"doc_title": "Board pack", "parent_uuid": "", "doc_private": true}).data()["doc_uuid"])
	postAs(t, ctx, people["olu"], "olu shares it", docController.UpdateDocAccess, map[string]any{"doc_uuid": doc, "adds": []any{
		map[string]any{"kind": "user", "uuid": uuidOf("ed"), "level": "edit"},
		map[string]any{"kind": "user", "uuid": uuidOf("zed"), "level": "edit", "until": time.Now().Add(time.Hour).UTC().Truncate(time.Second)},
	}})
	postAs(t, ctx, people["olu"], "olu hands it to bea", docController.TransferDoc, map[string]any{"doc_uuid": doc, "to_user_uuid": uuidOf("bea")})
	postAs(t, ctx, people["bea"], "bea takes olu off it", docController.UpdateDocAccess, map[string]any{"doc_uuid": doc, "removes": []any{map[string]any{"uuid": uuidOf("olu")}}})
	if _, err := env.PG.Exec(`UPDATE access_grant_expiry SET expires_at = NOW() - interval '1 minute' WHERE principal_uuid = $1`, uuidOf("zed")); err != nil {
		t.Fatal(err)
	}
	all := []string{uuidOf("olu"), uuidOf("bea"), uuidOf("ed"), uuidOf("zed")}
	readers, err := docBusiness.Readers(ctx, doc, all)
	if err != nil {
		t.Fatal(err)
	}
	if !readers[uuidOf("bea")] || !readers[uuidOf("ed")] || readers[uuidOf("olu")] || readers[uuidOf("zed")] {
		t.Fatalf("who may read the doc: %v, want bea and ed alone", readers)
	}

	t.Run("a guest's comment tells the keepers who may read the doc", func(t *testing.T) {
		// Read as the guest's path reads it (business/Guest notifyDocKeepers).
		bot, err := userBusiness.EnsureGuestBot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		d, err := docBusiness.GetBasicDgraphDocByUUID(ctx, doc, bot.DgraphUID)
		if err != nil || d == nil {
			t.Fatalf("read the doc as the guests: %v", err)
		}
		comment := uuid.NewString()
		commentBusiness.NotifyDocCommentFor(ctx, d, "Priya (guest)", comment, "Can the launch move to Friday?")
		key := firebaseInit.FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT + ":" + doc + ":" + comment
		if got := broker.notices(key, people); !slices.Equal(got, []string{"bea", "ed"}) {
			t.Errorf("the comment reached %v, want bea (its owner) and ed (its editor): olu was taken off it, and zed's grant has ended", got)
		}
		emailed := func(name string) bool {
			var n int
			_ = env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1 AND event_type = 'doc.comment'`, people[name].UserPostgresInfo.Id).Scan(&n)
			return n > 0
		}
		waitLong(t, "the keepers' emails", 20*time.Second, func() bool { return emailed("bea") && emailed("ed") })
		for _, name := range []string{"olu", "zed"} {
			if emailed(name) {
				t.Errorf("%s, who may not read the doc, was emailed the guest's comment", name)
			}
		}
	})

	t.Run("a restore that turned the doc's guest links off tells its owner, not its maker", func(t *testing.T) {
		bea := people["bea"]
		hash := sha256.Sum256([]byte(uuid.NewString()))
		if _, err := guestModel.CreateGrant(ctx, hash[:], guestModel.ResourceDoc, doc, guestModel.CapabilityComment, bea.UserPostgresInfo.Id, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := docBusiness.TrashSubtree(ctx, doc, bea.UserDgraphInfo.Uid, bea.UserDgraphInfo.Uuid, bea.UserPostgresInfo.EmailID); err != nil {
			t.Fatalf("bea deletes the doc: %v", err)
		}
		asSam := context.WithValue(ctx, helpers.UserInfoContextKey, people["sam"])
		if n, err := archiveBusiness.RestoreItems(asSam, "docs", []uuid.UUID{uuid.MustParse(doc)}); err != nil || n != 1 {
			t.Fatalf("sam restores it: %d, %v", n, err)
		}
		if got := broker.noticedFrom("doc_restored:"+doc+":", people); !slices.Equal(got, []string{"bea"}) {
			t.Errorf("the restore was pushed to %v, want bea, its owner: olu handed it on and was taken off it", got)
		}
	})
}
