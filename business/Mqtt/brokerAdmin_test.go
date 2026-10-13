package business

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// fakeBroker answers as EMQX's admin API does (v5 paths, as EMQX 6 serves
// them; checked against a real emqx:6.0.0): a login for a token, clients
// listed by username or id, subscriptions by topic, unsubscribe and kick.
type fakeBroker struct {
	t        *testing.T
	mu       sync.Mutex
	password string
	tokens   int               // tokens handed out
	expired  map[string]bool   // tokens it no longer takes
	clients  map[string]string // client id -> username
	subs     map[string][]string
	logins   int
	kicked   []string
	requests []string
}

func newFakeBroker(t *testing.T) (*fakeBroker, *httptest.Server) {
	f := &fakeBroker{t: t, password: "console-secret-123", expired: map[string]bool{}, clients: map[string]string{}, subs: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeBroker) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.URL.Path == "/api/v5/login" {
		f.logins++
		var in struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Username != "console" || in.Password != f.password {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":"BAD_USERNAME_OR_PWD"}`)
			return
		}
		f.tokens++
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok-" + string(rune('0'+f.tokens)), "role": "administrator"})
		return
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if auth == "" || f.expired[auth] || !strings.HasPrefix(auth, "tok-") {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"BAD_TOKEN"}`)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v5/clients":
		q := r.URL.Query()
		var data []map[string]any
		ids := q["clientid"]
		for id, user := range f.clients {
			if (len(q["username"]) > 0 && slices.Contains(q["username"], user)) || slices.Contains(ids, id) {
				data = append(data, map[string]any{"clientid": id, "username": user, "connected": true})
			}
		}
		slices.SortFunc(data, func(a, b map[string]any) int { return strings.Compare(a["clientid"].(string), b["clientid"].(string)) })
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": map[string]any{"hasnext": false}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v5/subscriptions":
		topic := r.URL.Query().Get("topic")
		var data []map[string]any
		for id, topics := range f.subs {
			if slices.Contains(topics, topic) {
				data = append(data, map[string]any{"clientid": id, "topic": topic, "qos": 1})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": map[string]any{"hasnext": false}})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/unsubscribe/bulk"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v5/clients/"), "/unsubscribe/bulk")
		if _, ok := f.clients[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"CLIENTID_NOT_FOUND"}`)
			return
		}
		var body []struct{ Topic string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, b := range body {
			f.subs[id] = slices.DeleteFunc(f.subs[id], func(t string) bool { return t == b.Topic })
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v5/clients/kickout/bulk":
		var ids []string
		_ = json.NewDecoder(r.Body).Decode(&ids)
		for _, id := range ids {
			f.kicked = append(f.kicked, id)
			delete(f.clients, id)
			delete(f.subs, id)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeBroker) connect(clientID, username string, topics ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clients[clientID] = username
	f.subs[clientID] = append([]string(nil), topics...)
}

func (f *fakeBroker) topicsOf(clientID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.subs[clientID]...)
}

func TestARemovedMembersClientsLoseTheConversationsTopics(t *testing.T) {
	t.Setenv("JWT_SECRET", "broker-admin-test")
	f, srv := newFakeBroker(t)
	a := newBrokerAdmin(srv.URL, "console", f.password)
	maya, jonas := "6f1c3b0e-0d6f-4c56-9a59-3a3f4e1c2b01", "0b8e1d7c-5a2f-4b9e-8c3d-2e1f0a9b8c02"
	msg, typing := helpers.GetMqttTopicForChannel("4a1d0c6e-7b8f-4e2d-9c1a-0f2e3d4c5b6a")
	other, _ := helpers.GetMqttTopicForChannel("9d8c7b6a-5f4e-4d3c-8b2a-1e0f9d8c7b6a")
	// Two tabs of Maya's, one of Jonas's, all on the channel.
	f.connect("c_dev1_6f1c3b0e_ab12", "user_"+maya, msg, typing, other)
	f.connect("c_dev2_6f1c3b0e_cd34", "user_"+maya, msg, typing)
	f.connect("c_dev3_0b8e1d7c_ef56", "user_"+jonas, msg, typing)

	if err := a.cutFromTopics(context.Background(), maya, topicsOf("channel", "4a1d0c6e-7b8f-4e2d-9c1a-0f2e3d4c5b6a")); err != nil {
		t.Fatalf("cut: %v", err)
	}
	if got := f.topicsOf("c_dev1_6f1c3b0e_ab12"); !slices.Equal(got, []string{other}) {
		t.Errorf("Maya's first tab still holds %v; only the other channel should be left", got)
	}
	if got := f.topicsOf("c_dev2_6f1c3b0e_cd34"); len(got) != 0 {
		t.Errorf("Maya's second tab still holds %v", got)
	}
	if got := f.topicsOf("c_dev3_0b8e1d7c_ef56"); len(got) != 2 {
		t.Errorf("Jonas, still a member, lost his topics: %v", got)
	}
	if f.logins != 1 {
		t.Errorf("%d logins for one cut; the token is kept", f.logins)
	}
}

func TestADeactivatedPersonsClientsAreDisconnected(t *testing.T) {
	f, srv := newFakeBroker(t)
	a := newBrokerAdmin(srv.URL, "console", f.password)
	maya := "6f1c3b0e-0d6f-4c56-9a59-3a3f4e1c2b01"
	f.connect("c_a", "user_"+maya, "message/x")
	f.connect("c_b", "user_"+maya)
	f.connect("c_c", "user_someone-else")
	if err := a.disconnect(context.Background(), maya); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	slices.Sort(f.kicked)
	if !slices.Equal(f.kicked, []string{"c_a", "c_b"}) {
		t.Fatalf("kicked %v, want Maya's two clients", f.kicked)
	}
}

func TestATokenTheBrokerNoLongerTakesIsReplaced(t *testing.T) {
	f, srv := newFakeBroker(t)
	a := newBrokerAdmin(srv.URL, "console", f.password)
	if _, err := a.clientsOf(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.expired["tok-1"] = true // tokens last an hour
	f.mu.Unlock()
	if _, err := a.clientsOf(context.Background(), "x"); err != nil {
		t.Fatalf("after the token expired: %v", err)
	}
	if f.logins != 2 {
		t.Fatalf("%d logins, want 2", f.logins)
	}
}

// A wrong console password is said once, never printed, and not tried again
// for a while: the console locks an account that keeps failing.
func TestARefusedLoginIsNotHammeredOrPrinted(t *testing.T) {
	f, srv := newFakeBroker(t)
	a := newBrokerAdmin(srv.URL, "console", "a-wrong-password-456")
	_, err := a.clientsOf(context.Background(), "x")
	if !errors.Is(err, errBrokerLoginRefused) {
		t.Fatalf("err %v, want a refused login", err)
	}
	if strings.Contains(err.Error(), "a-wrong-password-456") || strings.Contains(err.Error(), "console") {
		t.Fatalf("the error names the login: %v", err)
	}
	for i := 0; i < 3; i++ {
		_, _ = a.clientsOf(context.Background(), "x")
	}
	if f.logins != 1 {
		t.Fatalf("%d logins after a refusal, want 1", f.logins)
	}
}

func TestAnErrorFromTheBrokerNamesNoSecret(t *testing.T) {
	a := newBrokerAdmin("http://127.0.0.1:1", "console", "never-printed-789")
	_, err := a.clientsOf(context.Background(), "x")
	if err == nil {
		t.Fatal("no error from a broker that isn't there")
	}
	if strings.Contains(err.Error(), "never-printed-789") {
		t.Fatalf("the error names the password: %v", err)
	}
}

// A channel made private: every client on its topics whose person isn't a
// member loses them; members, and clients that aren't a person's, keep them.
func TestARecheckCutsOnlyThoseNoLongerIn(t *testing.T) {
	t.Setenv("JWT_SECRET", "broker-admin-test")
	f, srv := newFakeBroker(t)
	a := newBrokerAdmin(srv.URL, "console", f.password)
	member, removed := "6f1c3b0e-0d6f-4c56-9a59-3a3f4e1c2b01", "0b8e1d7c-5a2f-4b9e-8c3d-2e1f0a9b8c02"
	msg, typing := helpers.GetMqttTopicForChannel("4a1d0c6e-7b8f-4e2d-9c1a-0f2e3d4c5b6a")
	f.connect("c_member", "user_"+member, msg, typing)
	f.connect("c_removed", "user_"+removed, msg, typing)
	f.connect("backend", "backend", msg)

	stillReads := func(_ context.Context, user, topic string) (bool, error) {
		return user == member, nil
	}
	if err := a.recheck(context.Background(), []string{msg, typing}, stillReads); err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if got := f.topicsOf("c_removed"); len(got) != 0 {
		t.Errorf("the removed person's client still holds %v", got)
	}
	if got := f.topicsOf("c_member"); len(got) != 2 {
		t.Errorf("the member's client lost its topics: %v", got)
	}
	if got := f.topicsOf("backend"); len(got) != 1 {
		t.Errorf("the server's own client lost its topic: %v", got)
	}
}

func TestTheAdminAPIIsBesideTheBroker(t *testing.T) {
	t.Setenv("EMQX_API_URL", "")
	t.Setenv("MQTT_BROKER", "tcp://emqx:1883")
	if got := brokerAPIURL(); got != "http://emqx:18083" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("EMQX_API_URL", "http://broker.internal:18083/")
	if got := brokerAPIURL(); got != "http://broker.internal:18083/" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("EMQX_DASHBOARD_DEFAULT_USERNAME", "")
	t.Setenv("EMQX_DASHBOARD_DEFAULT_PASSWORD", "")
	if theBrokerAdmin() != nil {
		t.Fatal("an admin client with no login")
	}
	t.Setenv("EMQX_DASHBOARD_DEFAULT_USERNAME", "console")
	t.Setenv("EMQX_DASHBOARD_DEFAULT_PASSWORD", "pw")
	a := theBrokerAdmin()
	if a == nil || a.base != "http://broker.internal:18083/api/v5" || theBrokerAdmin() != a {
		t.Fatalf("admin client %+v", a)
	}
}

// Taken out of a channel: the broker's next question about its topic is
// answered from a fresh read, and a read that began before the change is
// never kept.
func TestForgottenTopicsAreReadAgain(t *testing.T) {
	t.Setenv("JWT_SECRET", "broker-admin-test")
	person := "6f1c3b0e-0d6f-4c56-9a59-3a3f4e1c2b01"
	channel := "4a1d0c6e-7b8f-4e2d-9c1a-0f2e3d4c5b6a"
	msg, _ := helpers.GetMqttTopicForChannel(channel)
	member := true
	reads := 0
	prev := readPersonTopics
	readPersonTopics = func(context.Context, string) (*dgraphStruct.DgraphUser, error) {
		reads++
		u := &dgraphStruct.DgraphUser{}
		if member {
			u.Channels = []*dgraphStruct.DgraphChannel{{Uuid: channel}}
		}
		return u, nil
	}
	t.Cleanup(func() {
		readPersonTopics = prev
		memberTopicsCache.Delete(person)
		memberTopicsForgotten.Delete(person)
	})

	if in, err := IsMemberTopic(context.Background(), person, msg); err != nil || !in {
		t.Fatalf("a member's channel: %v %v", in, err)
	}
	member = false
	ForgetMemberTopics(person)
	if in, _ := IsMemberTopic(context.Background(), person, msg); in {
		t.Fatal("a channel just left can still be subscribed to")
	}
	if reads != 2 {
		t.Fatalf("%d reads, want 2", reads)
	}

	// A read that began before the person was taken out isn't kept.
	memberTopicsCache.Delete(person)
	started, proceed, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	readPersonTopics = func(context.Context, string) (*dgraphStruct.DgraphUser, error) {
		close(started)
		<-proceed
		return &dgraphStruct.DgraphUser{Channels: []*dgraphStruct.DgraphChannel{{Uuid: channel}}}, nil
	}
	go func() {
		_, _ = readMemberTopics(context.Background(), person)
		close(done)
	}()
	<-started
	time.Sleep(time.Millisecond)
	ForgetMemberTopics(person)
	close(proceed)
	<-done
	if _, kept := memberTopicsCache.Get(person); kept {
		t.Fatal("a read that began before the person was taken out was kept")
	}
}
