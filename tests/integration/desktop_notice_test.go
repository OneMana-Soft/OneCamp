//go:build integration
// +build integration

package integration_test

// The desktop app's notices, against real Postgres, Dgraph and Redis
// (TN-BUG-12).
//
// The desktop app's webview has no web push, so it registers no device, and a
// person who used only the desktop app was told of nothing. Every push now goes
// out with a desktop notice on each recipient's own activity topic, decided by
// person, not by device:
//   - a DM reaches the person it's to with no device at all;
//   - a channel message reaches its members by their setting, a member with
//     no device included, and those it mentions who are in the channel;
//   - never its writer, someone paused or in focus time, a member who muted
//     the channel, someone mentioned from outside a private channel, or
//     someone deactivated (even by Notify anyway);
//   - and when the push fails, the email after it still goes.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDesktopNoticesReachWhomAPushWould -v

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	userAdapter "github.com/akashc777/OneCamp/adapter/User"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// noticeBroker is the MQTT broker as the server sees it: every publish, kept.
type noticeBroker struct {
	MQTT.Client
	mu  sync.Mutex
	got []struct {
		topic   string
		payload []byte
	}
}

func (b *noticeBroker) Publish(topic string, _ byte, _ bool, payload interface{}) MQTT.Token {
	p, _ := payload.([]byte)
	b.mu.Lock()
	b.got = append(b.got, struct {
		topic   string
		payload []byte
	}{topic, p})
	b.mu.Unlock()
	return doneMqttToken{}
}

type doneMqttToken struct{}

func (doneMqttToken) Wait() bool                     { return true }
func (doneMqttToken) WaitTimeout(time.Duration) bool { return true }
func (doneMqttToken) Done() <-chan struct{}          { c := make(chan struct{}); close(c); return c }
func (doneMqttToken) Error() error                   { return nil }

// notices is who, of people, was sent a desktop notice with this id.
func (b *noticeBroker) notices(noticeID string, people map[string]userModels.UserInfo) []string {
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
				msg.Type == mqttStruct.MESSAGE_DESKTOP_NOTICE && msg.Data.NoticeID == noticeID {
				who = append(who, name)
			}
		}
	}
	slices.Sort(who)
	return who
}

func TestDesktopNoticesReachWhomAPushWould(t *testing.T) {
	t.Setenv("JWT_SECRET", "desktop-notice-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.SetupDgraph(t)
	broker := &noticeBroker{}
	mqttInit.MqttClient = broker
	t.Cleanup(func() { mqttInit.MqttClient = nil })
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	settingsBusiness.ForgetSettingsForTest()
	t.Cleanup(settingsBusiness.ForgetSettingsForTest)

	// Push is set up, and every send fails: the credential's token server
	// answers 500, as an outage would, so no send gets as far as FCM. Nothing
	// here reaches the internet.
	var authTries sync.WaitGroup
	authTries.Add(1)
	var once sync.Once
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(authTries.Done)
		http.Error(w, "outage", http.StatusInternalServerError)
	}))
	t.Cleanup(tokenServer.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cred, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "onecamp-test", "private_key_id": "k1", "private_key": string(pemKey),
		"client_email": "push@onecamp-test.iam.gserviceaccount.com", "client_id": "1",
		"token_uri": tokenServer.URL,
	})
	if err := firebaseInit.ConnectFirebase(&firebaseInit.FirebaseAppConfigStruct{FirebaseCredJSON: string(cred)}); err != nil {
		t.Fatalf("set push up: %v", err)
	}
	t.Cleanup(func() { _ = firebaseInit.ConnectFirebase(&firebaseInit.FirebaseAppConfigStruct{}) })
	t.Setenv("RESEND_API_KEY", "re_test_never_sent")

	var pmu sync.Mutex
	pushedTo := map[string]bool{}
	pushedFor := map[string][]string{} // a push's thread_id: the devices it was for
	t.Cleanup(firebaseInit.ObservePushesForTest(func(data map[string]string, tokens []string) {
		pmu.Lock()
		for _, tok := range tokens {
			pushedTo[tok] = true
		}
		thread := data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID]
		pushedFor[thread] = append(pushedFor[thread], tokens...)
		pmu.Unlock()
	}))
	pushed := func(tok string) bool {
		pmu.Lock()
		defer pmu.Unlock()
		return pushedTo[tok]
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	people := map[string]userModels.UserInfo{}
	for _, name := range []string{"maya", "ana", "bo", "moe", "paul", "fay", "nina", "dex"} {
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
		if name != "ana" { // ana uses only the desktop app: no device
			exec(`INSERT INTO users_fcm_token (user_id, device_id, fcm_token) VALUES ($1, 'phone', $2)`, id, "tok-"+name)
		}
		exec(`INSERT INTO users_notification_preferences (user_id, unsubscribe_token) VALUES ($1, $2) ON CONFLICT (user_id) DO NOTHING`, id, uuid.NewString())
	}
	id := func(name string) uuid.UUID { return people[name].UserPostgresInfo.Id }
	exec(`UPDATE users_notification_preferences SET notifications_paused_until = NOW() + INTERVAL '1 hour' WHERE user_id = $1`, id("paul"))
	exec(`INSERT INTO calendar_events (title, start_time, end_time, created_by, is_focus) VALUES ('Deep work', NOW() - INTERVAL '10 minutes', NOW() + INTERVAL '50 minutes', $1, true)`, id("fay"))
	// dex was deactivated: their settings and their phone's token stay, as
	// deactivation leaves them.
	if err := userBusiness.DeactivateUser(ctx, id("dex")); err != nil {
		t.Fatalf("deactivate dex: %v", err)
	}
	maya := people["maya"]
	all := map[string]userModels.UserInfo{}
	for name, p := range people {
		all[name] = p
	}

	t.Run("a DM reaches someone with no device, and the push gate holds the rest", func(t *testing.T) {
		dm := func(to string) string {
			t.Helper()
			m, err := sendBusiness.PrepareDirectMessage(ctx, &maya, &chatAdapter.ChatInfo{TextHtml: "<p>Lunch?</p>", ToUuid: people[to].UserDgraphInfo.Uuid})
			if err != nil {
				t.Fatalf("DM %s: %v", to, err)
			}
			out, err := m.Commit(ctx)
			if err != nil {
				t.Fatalf("DM %s: %v", to, err)
			}
			return "chat:" + helpers.GetGroupingId(maya.UserDgraphInfo.Uuid, people[to].UserDgraphInfo.Uuid) + ":" + out.Uuid
		}
		toAna, toPaul, toFay := dm("ana"), dm("paul"), dm("fay")
		waitLong(t, "ana's notice", 20*time.Second, func() bool { return len(broker.notices(toAna, all)) == 1 })
		time.Sleep(time.Second) // what else was coming has come
		if got := broker.notices(toAna, all); !slices.Equal(got, []string{"ana"}) {
			t.Errorf("the DM to ana was noticed by %v, want ana alone (never maya, who wrote it)", got)
		}
		if got := broker.notices(toPaul, all); len(got) != 0 || pushed("tok-paul") {
			t.Errorf("paul, paused, was noticed %v / pushed %v", got, pushed("tok-paul"))
		}
		if got := broker.notices(toFay, all); len(got) != 0 || pushed("tok-fay") {
			t.Errorf("fay, in focus time, was noticed %v / pushed %v", got, pushed("tok-fay"))
		}
	})

	t.Run("a channel message reaches its members by their setting, and no one outside", func(t *testing.T) {
		err, ch := channelBusiness.CreateChannel(ctx, &channelAdapter.InputCreateChannel{ChannelName: "launch-pricing", ChannelPrivate: true}, &maya)
		if err != nil {
			t.Fatalf("make #launch-pricing: %v", err)
		}
		members := []string{"ana", "bo", "moe", "paul", "fay"}
		for _, name := range members {
			p := people[name]
			if err := channelBusiness.AddChannelMemberEdge(ctx, ch, &p.UserDgraphInfo, id(name)); err != nil {
				t.Fatalf("add %s: %v", name, err)
			}
		}
		waitLong(t, "the members' settings", 20*time.Second, func() bool {
			var n int
			_ = env.PG.QueryRow(`SELECT count(*) FROM users_channel_notification WHERE channel_id = $1`, ch).Scan(&n)
			return n >= len(members)
		})
		exec(`UPDATE users_channel_notification SET notification_type = 'mention' WHERE channel_id = $1 AND user_id = $2`, ch, id("bo"))
		exec(`UPDATE users_channel_notification SET notification_type = 'block' WHERE channel_id = $1 AND user_id = $2`, ch, id("moe"))

		mention := func(name string) string {
			p := people[name]
			return fmt.Sprintf(`<span data-type="mention" data-id="%s@%s">@%s</span>`, p.UserDgraphInfo.Uuid, p.UserDgraphInfo.Uid, name)
		}
		post, err := sendBusiness.PrepareChannelPost(ctx, &maya, &postAdapter.InputCreateOrUpdatePostInfo{
			HTMLText:    "<p>" + mention("bo") + " " + mention("nina") + " the new prices are in</p>",
			ChannelUuid: ch.String(),
		})
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		created, err := post.Commit(ctx)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		noticeID := "channel:" + ch.String() + ":" + created.Uuid
		waitLong(t, "the channel's notices", 20*time.Second, func() bool { return len(broker.notices(noticeID, all)) >= 2 })
		time.Sleep(time.Second)
		if got := broker.notices(noticeID, all); !slices.Equal(got, []string{"ana", "bo"}) {
			t.Errorf("noticed by %v, want ana (all, no device) and bo (mention, mentioned): never maya, "+
				"moe (muted), paul (paused), fay (focus) or nina (mentioned, not a member)", got)
		}
		for _, held := range []string{"tok-maya", "tok-moe", "tok-paul", "tok-fay", "tok-nina"} {
			if pushed(held) {
				t.Errorf("%s was pushed", held)
			}
		}
		if !pushed("tok-bo") {
			t.Error("bo's device wasn't pushed")
		}

		// Forwarded into a DM with ana, it is told of as a new message there:
		// her desktop app gets the notice (BE-11-G-2), which a forward's own
		// push loop never sent.
		broker.mu.Lock()
		before := len(broker.got) // the DM to ana above shares the conversation's prefix
		broker.mu.Unlock()
		raw, _ := json.Marshal(userAdapter.UserFwdMsgInput{
			HtmlText: "<p>the new prices are in</p>", ChannelUuid: ch.String(), PostUuid: created.Uuid,
			FwdTo: []userAdapter.UserAndChannelFwdMessage{{UserUuid: people["ana"].UserDgraphInfo.Uuid,
				UserDgraphUid: people["ana"].UserDgraphInfo.Uid, UserName: "ana"}},
		})
		rec := httptest.NewRecorder()
		userController.FwdUserMessage(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(
			context.WithValue(ctx, helpers.UserInfoContextKey, maya)))
		if rec.Code != http.StatusOK {
			t.Fatalf("maya's forward: %d %s", rec.Code, rec.Body.String())
		}
		prefix := "chat:" + helpers.GetGroupingId(maya.UserDgraphInfo.Uuid, people["ana"].UserDgraphInfo.Uuid) + ":"
		forwarded := func() (who []string) {
			broker.mu.Lock()
			defer broker.mu.Unlock()
			for name, p := range all {
				topic := helpers.GetMqttTopicForUserActivity(p.UserDgraphInfo.Uuid)
				for _, m := range broker.got[before:] {
					var msg struct {
						Type int8                         `json:"type"`
						Data mqttStruct.MqttDesktopNotice `json:"data"`
					}
					if m.topic == topic && json.Unmarshal(m.payload, &msg) == nil && msg.Type == mqttStruct.MESSAGE_DESKTOP_NOTICE &&
						strings.HasPrefix(msg.Data.NoticeID, prefix) {
						who = append(who, name)
					}
				}
			}
			return who
		}
		waitLong(t, "the forward's notice", 20*time.Second, func() bool { return len(forwarded()) > 0 })
		time.Sleep(time.Second)
		if got := forwarded(); !slices.Equal(got, []string{"ana"}) {
			t.Errorf("the forward was noticed by %v, want ana alone (never maya, who sent it)", got)
		}
	})

	t.Run("someone deactivated is told nothing, even through a pause", func(t *testing.T) {
		to := []string{id("bo").String(), id("dex").String()}
		data := map[string]string{firebaseInit.FIREBASE_PUSH_DATA_TITLE: "Maya", firebaseInit.FIREBASE_PUSH_DATA_BODY: "Board pack attached"}
		notificationBusiness.Deliver(ctx, to, data, "test:deactivated:1")
		_ = notificationBusiness.DeliverThroughPause(ctx, to, maps.Clone(data), "test:deactivated:2")
		for _, n := range []string{"test:deactivated:1", "test:deactivated:2"} {
			if got := broker.notices(n, all); !slices.Equal(got, []string{"bo"}) {
				t.Errorf("%s was noticed by %v, want bo alone: dex is deactivated", n, got)
			}
		}
		if pushed("tok-dex") {
			t.Error("dex, deactivated, was pushed")
		}
	})

	t.Run("when the push fails, the email after it still goes", func(t *testing.T) {
		m, err := sendBusiness.PrepareDirectMessage(ctx, &maya, &chatAdapter.ChatInfo{TextHtml: "<p>Can you sign this today?</p>", ToUuid: people["bo"].UserDgraphInfo.Uuid})
		if err != nil {
			t.Fatal(err)
		}
		out, err := m.Commit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		waitLong(t, "bo's email, after a push that failed", 20*time.Second, func() bool {
			var n int
			_ = env.PG.QueryRow(`SELECT count(*) FROM notification_email_queue WHERE user_id = $1 AND event_type = 'chat.dm'`, id("bo")).Scan(&n)
			return n >= 1
		})
		pmu.Lock()
		devices := slices.Clone(pushedFor[out.Uuid])
		pmu.Unlock()
		if !slices.Equal(devices, []string{"tok-bo"}) {
			t.Errorf("the DM's push was for %v, want bo's device", devices)
		}
		// And the sends failed: they got as far as asking for a token.
		failed := make(chan struct{})
		go func() { authTries.Wait(); close(failed) }()
		select {
		case <-failed:
		case <-time.After(20 * time.Second):
			t.Error("no send asked for a token, so none failed")
		}
	})
}
