package business

// What the broker can't learn by asking: that someone's access ended.
//
// The broker asks this server before each subscription a person's client
// makes (business/MqttAccess), and never again about one the client holds.
// So removing someone from a private channel stopped their next subscription
// and left the one their open app already had: every post, edit, reaction
// and typing in that channel kept streaming to it until it reconnected. A
// client resuming its session (the web app's sessions outlive a disconnect
// by an hour) got its subscriptions back without being asked about them, and
// a deactivated person's open session kept everything.
//
// So access that ends is also cut at the broker, through its admin API
// (EMQX's management REST API, which EMQX 6 serves as v5):
//   - removed from a channel, or left it: that person's clients lose its
//     topics (DropConversation);
//   - deactivated: their clients are disconnected and their sessions
//     discarded (DisconnectPerson);
//   - a channel made private: each client still on its topics whose person
//     isn't a member loses them (RecheckTopicSubscribers), a net under a cut
//     that failed.
//
// Each runs in the background and is tried again a few times, further apart,
// then logged: nothing waits for the broker, and nothing fails because it
// didn't answer. The login is the broker console's, from the configuration
// every install has (EMQX_DASHBOARD_DEFAULT_USERNAME and _PASSWORD), and
// never appears in a log line or an error.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

var (
	// errBrokerLoginRefused: the broker refused the console login. Not asked
	// again for brokerLoginRetryAfter, so a wrong password never locks the
	// console's account. (A broker starting up refuses it too, for a moment
	// after it says it's up: the next try at a cut asks again.)
	errBrokerLoginRefused = errors.New("the broker refused its admin login")
	// errBrokerNotFound: the broker has no such client (gone since it was
	// listed, say).
	errBrokerNotFound = errors.New("the broker has no such client")
)

const (
	brokerLoginRetryAfter = time.Minute
	// brokerPageSize and brokerMaxPages bound a listing: one person's
	// clients, or one topic's subscribers.
	brokerPageSize = 100
	brokerMaxPages = 50
)

// brokerAdmin calls the broker's admin API.
type brokerAdmin struct {
	base     string // http://emqx:18083/api/v5
	username string
	password string
	client   *http.Client

	mu        sync.Mutex
	token     string
	refusedAt time.Time
}

func newBrokerAdmin(apiURL, username, password string) *brokerAdmin {
	return &brokerAdmin{
		base:     strings.TrimRight(apiURL, "/") + "/api/v5",
		username: username,
		password: password,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// brokerAPIURL is where the broker's admin API is: EMQX_API_URL, else port
// 18083 of the host the server publishes to (MQTT_BROKER tcp://emqx:1883
// gives http://emqx:18083). The console port is never published outside the
// compose network; the server reaches it inside it.
func brokerAPIURL() string {
	if u := strings.TrimSpace(os.Getenv("EMQX_API_URL")); u != "" {
		return u
	}
	broker, err := url.Parse(strings.TrimSpace(os.Getenv("MQTT_BROKER")))
	if err != nil || broker.Hostname() == "" {
		return ""
	}
	return "http://" + net.JoinHostPort(broker.Hostname(), "18083")
}

var (
	brokerAdminMu      sync.Mutex
	brokerAdminCurrent *brokerAdmin
	brokerAdminUnset   sync.Once
)

// theBrokerAdmin is the admin API client for the configuration as it is
// now, or nil when there's no login to use.
func theBrokerAdmin() *brokerAdmin {
	apiURL := brokerAPIURL()
	user := os.Getenv("EMQX_DASHBOARD_DEFAULT_USERNAME")
	pass := os.Getenv("EMQX_DASHBOARD_DEFAULT_PASSWORD")
	if apiURL == "" || user == "" || pass == "" {
		brokerAdminUnset.Do(func() {
			helpers.LogWarnWithContext(context.Background(),
				"business/Mqtt the broker's admin login isn't set (EMQX_DASHBOARD_DEFAULT_USERNAME, "+
					"EMQX_DASHBOARD_DEFAULT_PASSWORD, MQTT_BROKER or EMQX_API_URL): someone removed from a "+
					"channel, or deactivated, keeps its live messages in an app already open until it reconnects")
		})
		return nil
	}
	brokerAdminMu.Lock()
	defer brokerAdminMu.Unlock()
	a := brokerAdminCurrent
	if a == nil || a.base != strings.TrimRight(apiURL, "/")+"/api/v5" || a.username != user || a.password != pass {
		a = newBrokerAdmin(apiURL, user, pass)
		brokerAdminCurrent = a
	}
	return a
}

// login asks the broker for a token. The caller holds a.mu.
func (a *brokerAdmin) login(ctx context.Context) (string, error) {
	if !a.refusedAt.IsZero() && time.Since(a.refusedAt) < brokerLoginRetryAfter {
		return "", errBrokerLoginRefused
	}
	body, _ := json.Marshal(map[string]string{"username": a.username, "password": a.password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/login", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("broker admin login: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("broker admin login: %w", redactURLError(err))
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		a.refusedAt = time.Now()
		return "", errBrokerLoginRefused
	}
	if res.StatusCode/100 != 2 {
		return "", fmt.Errorf("broker admin login: status %d", res.StatusCode)
	}
	var answer struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&answer); err != nil || answer.Token == "" {
		return "", errors.New("broker admin login: no token in the answer")
	}
	a.refusedAt = time.Time{}
	return answer.Token, nil
}

// authToken is the token to call with, logging in for one when there's none.
func (a *brokerAdmin) authToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" {
		return a.token, nil
	}
	token, err := a.login(ctx)
	if err != nil {
		return "", err
	}
	a.token = token
	return token, nil
}

// dropToken forgets a token the broker no longer takes (they last an hour).
func (a *brokerAdmin) dropToken(token string) {
	a.mu.Lock()
	if a.token == token {
		a.token = ""
	}
	a.mu.Unlock()
}

// call sends one request to the admin API and reads its JSON answer into out
// (when out isn't nil). A token the broker no longer takes is replaced once.
func (a *brokerAdmin) call(ctx context.Context, method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		token, err := a.authToken(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, a.base+path, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("broker admin %s: %w", method, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := a.client.Do(req)
		if err != nil {
			return fmt.Errorf("broker admin %s %s: %w", method, pathOnly(path), redactURLError(err))
		}
		answer, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close()
		switch {
		case res.StatusCode == http.StatusUnauthorized && attempt == 0:
			a.dropToken(token)
			continue
		case res.StatusCode == http.StatusNotFound:
			return errBrokerNotFound
		case res.StatusCode/100 != 2:
			var e struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(answer, &e)
			return fmt.Errorf("broker admin %s %s: status %d %s", method, pathOnly(path), res.StatusCode, e.Code)
		}
		if out == nil || len(answer) == 0 {
			return nil
		}
		if err := json.Unmarshal(answer, out); err != nil {
			return fmt.Errorf("broker admin %s %s: %w", method, pathOnly(path), err)
		}
		return nil
	}
}

// pathOnly leaves the query out of a path that goes in an error.
func pathOnly(path string) string {
	p, _, _ := strings.Cut(path, "?")
	return p
}

// redactURLError keeps a transport error's cause and drops the URL it names.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// brokerClient is one client in a listing.
type brokerClient struct {
	ClientID string `json:"clientid"`
	Username string `json:"username"`
}

// clients lists clients, page by page, for a query (username=, clientid=).
func (a *brokerAdmin) clients(ctx context.Context, query url.Values) ([]brokerClient, error) {
	var all []brokerClient
	for page := 1; page <= brokerMaxPages; page++ {
		query.Set("limit", strconv.Itoa(brokerPageSize))
		query.Set("page", strconv.Itoa(page))
		var answer struct {
			Data []brokerClient `json:"data"`
			Meta struct {
				HasNext bool `json:"hasnext"`
			} `json:"meta"`
		}
		if err := a.call(ctx, http.MethodGet, "/clients?"+query.Encode(), nil, &answer); err != nil {
			return nil, err
		}
		all = append(all, answer.Data...)
		if !answer.Meta.HasNext {
			break
		}
	}
	return all, nil
}

// clientsOf lists the ids of a person's clients: connected, or holding a
// session to resume.
func (a *brokerAdmin) clientsOf(ctx context.Context, userUUID string) ([]string, error) {
	username := "user_" + userUUID
	found, err := a.clients(ctx, url.Values{"username": {username}})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(found))
	for _, c := range found {
		if c.Username == username && c.ClientID != "" {
			ids = append(ids, c.ClientID)
		}
	}
	return ids, nil
}

// unsubscribe takes topics off one client, connected or holding a session.
// A client gone since it was listed is fine: it holds nothing.
func (a *brokerAdmin) unsubscribe(ctx context.Context, clientID string, topics []string) error {
	body := make([]map[string]string, 0, len(topics))
	for _, t := range topics {
		body = append(body, map[string]string{"topic": t})
	}
	err := a.call(ctx, http.MethodPost, "/clients/"+url.PathEscape(clientID)+"/unsubscribe/bulk", body, nil)
	if errors.Is(err, errBrokerNotFound) {
		return nil
	}
	return err
}

// kick disconnects clients and discards their sessions, subscriptions and
// all: a client that comes back starts afresh, asked about each topic again.
func (a *brokerAdmin) kick(ctx context.Context, clientIDs []string) error {
	if len(clientIDs) == 0 {
		return nil
	}
	return a.call(ctx, http.MethodPost, "/clients/kickout/bulk", clientIDs, nil)
}

// subscribersOf lists the ids of the clients subscribed to a topic.
func (a *brokerAdmin) subscribersOf(ctx context.Context, topic string) ([]string, error) {
	var ids []string
	for page := 1; page <= brokerMaxPages; page++ {
		query := url.Values{"topic": {topic}, "limit": {strconv.Itoa(brokerPageSize)}, "page": {strconv.Itoa(page)}}
		var answer struct {
			Data []struct {
				ClientID string `json:"clientid"`
				Topic    string `json:"topic"`
			} `json:"data"`
			Meta struct {
				HasNext bool `json:"hasnext"`
			} `json:"meta"`
		}
		if err := a.call(ctx, http.MethodGet, "/subscriptions?"+query.Encode(), nil, &answer); err != nil {
			return nil, err
		}
		for _, s := range answer.Data {
			if s.Topic == topic && s.ClientID != "" {
				ids = append(ids, s.ClientID)
			}
		}
		if !answer.Meta.HasNext {
			break
		}
	}
	return ids, nil
}

// cutFromTopics takes topics off every client of the person's.
func (a *brokerAdmin) cutFromTopics(ctx context.Context, userUUID string, topics []string) error {
	ids, err := a.clientsOf(ctx, userUUID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := a.unsubscribe(ctx, id, topics); err != nil {
			return err
		}
	}
	return nil
}

// disconnect disconnects every client of the person's, sessions and all.
func (a *brokerAdmin) disconnect(ctx context.Context, userUUID string) error {
	ids, err := a.clientsOf(ctx, userUUID)
	if err != nil {
		return err
	}
	return a.kick(ctx, ids)
}

// recheck takes each topic off every client on it whose person may no longer
// read it. stillReads answers for one person and one topic.
func (a *brokerAdmin) recheck(ctx context.Context, topics []string, stillReads func(ctx context.Context, userUUID, topic string) (bool, error)) error {
	for _, topic := range topics {
		ids, err := a.subscribersOf(ctx, topic)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			continue
		}
		query := url.Values{}
		for _, id := range ids {
			query.Add("clientid", id)
		}
		found, err := a.clients(ctx, query)
		if err != nil {
			return err
		}
		for _, c := range found {
			person, ok := helpers.ParseMqttUsername(c.Username)
			if !ok {
				// The server's own client, or anything not a person's.
				continue
			}
			reads, err := stillReads(ctx, person.String(), topic)
			if err != nil {
				return err
			}
			if !reads {
				if err := a.unsubscribe(ctx, c.ClientID, []string{topic}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// brokerRetryDelays are the waits before each try at a cut: at once, then
// further apart, for a broker restarting or a moment unreachable.
var brokerRetryDelays = []time.Duration{0, 2 * time.Second, 10 * time.Second, time.Minute, 5 * time.Minute}

// atTheBroker runs one cut in the background until it succeeds or its tries
// are spent, and says so in the log when they are.
func atTheBroker(what string, cut func(ctx context.Context, a *brokerAdmin) error) {
	a := theBrokerAdmin()
	if a == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				helpers.LogErrorWithContext(context.Background(), "business/Mqtt panic while trying to %s at the broker: %v", what, r)
			}
		}()
		var err error
		for _, wait := range brokerRetryDelays {
			time.Sleep(wait)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err = cut(ctx, a)
			cancel()
			if err == nil {
				return
			}
		}
		helpers.LogErrorWithContext(context.Background(),
			"business/Mqtt could not %s at the broker after %d tries, the last err: %v", what, len(brokerRetryDelays), err)
	}()
}

// topicsOf is a conversation's topics, by kind.
func topicsOf(kind, id string) []string {
	if kind == mqttStruct.CONVERSATION_KIND_CHANNEL {
		msg, typing := helpers.GetMqttTopicForChannel(id)
		return []string{msg, typing}
	}
	msg, typing := helpers.GetMqttTopicForDm(id)
	return []string{msg, typing}
}

// DropConversation ends a person's live access to a conversation they were
// taken out of: the broker refuses them its topics from now on, the clients
// they have lose the topics, and their open apps are told (so they let go of
// it and say so).
func DropConversation(kind, id, userUUID string) {
	if id == "" || userUUID == "" {
		return
	}
	ForgetMemberTopics(userUUID)
	topics := topicsOf(kind, id)
	go PublishConversationAccess(kind, id, mqttStruct.CONVERSATION_DROPPED, userUUID)
	atTheBroker("take "+kind+" "+id+" from "+userUUID, func(ctx context.Context, a *brokerAdmin) error {
		return a.cutFromTopics(ctx, userUUID, topics)
	})
}

// DisconnectPerson ends a deactivated person's live access: the broker
// refuses them every topic from now on, and their clients are disconnected,
// sessions discarded.
func DisconnectPerson(userUUID string) {
	if userUUID == "" {
		return
	}
	ForgetMemberTopics(userUUID)
	atTheBroker("disconnect "+userUUID, func(ctx context.Context, a *brokerAdmin) error {
		return a.disconnect(ctx, userUUID)
	})
}

// RecheckTopicSubscribers takes message and typing topics off every client
// on them whose person isn't in the conversation now, as the broker would
// answer if it were asked again.
func RecheckTopicSubscribers(topics ...string) {
	if len(topics) == 0 {
		return
	}
	atTheBroker("recheck "+strconv.Itoa(len(topics))+" topics' subscribers", func(ctx context.Context, a *brokerAdmin) error {
		return a.recheck(ctx, topics, stillMemberTopic)
	})
}

// stillMemberTopic answers from a fresh read of the person's conversations.
func stillMemberTopic(ctx context.Context, userUUID, topic string) (bool, error) {
	mt, err := readMemberTopics(ctx, userUUID)
	if err != nil {
		return false, err
	}
	_, in := mt.topics[topic]
	return in, nil
}
