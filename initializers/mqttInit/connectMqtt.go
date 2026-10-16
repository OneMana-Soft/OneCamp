package mqttInit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type MqttConfig struct {
	Broker       string
	ClientID     string
	Username     string
	CleanSession bool
}

type mqttClientConnect struct {
	ClientUserName string `json:"username"`
}

var MqttClient MQTT.Client
var userLocks sync.Map

func getUserLock(userID string) *sync.Mutex {
	lock, _ := userLocks.LoadOrStore(userID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// How the broker connection is made and kept. Variables so tests can shorten
// them; nothing else changes them.
var (
	// connectAttemptTimeout bounds one CONNECT: dialling, TLS and the CONNACK.
	connectAttemptTimeout = 10 * time.Second
	// connectBackoffBase and connectBackoffMax are the pauses between tries of
	// the first connection (helpers.Backoff: doubling, with jitter), and the
	// most the library waits between tries of any later reconnection.
	connectBackoffBase = time.Second
	connectBackoffMax  = 30 * time.Second
	// bootConnectWait is how long start-up waits for the first connection
	// before it carries on and leaves it to the background.
	bootConnectWait = 20 * time.Second
	// backgroundConnect bounds the background tries: for the life of the
	// process, so they end only when they succeed.
	backgroundConnect = context.Background()
	// keepAlive is how long the client stays quiet before it pings the
	// broker, and pingTimeout how long it waits for the answer before it
	// takes the connection for lost and reconnects. A connection can die
	// without a word: the machine sleeps, or the broker stops answering, and
	// nothing closes the socket. The ping is how the client finds out.
	keepAlive   = 30 * time.Second
	pingTimeout = 10 * time.Second
)

// connectTimings are the timings one effort to connect runs with, read from
// the variables above once, when it starts. An effort left trying in the
// background then reads nothing that a test puts back afterwards, so nothing
// in the server has to exist only for a test to wait on it.
type connectTimings struct {
	attempt   time.Duration // connectAttemptTimeout
	base, max time.Duration // connectBackoffBase, connectBackoffMax
}

func currentConnectTimings() connectTimings {
	return connectTimings{attempt: connectAttemptTimeout, base: connectBackoffBase, max: connectBackoffMax}
}

// ConnectMqtt connects the server's broker client, and keeps it connected.
//
// WHAT WENT WRONG. A failed first CONNECT was logged and then returned as
// success: the error was never assigned to the result. The library reconnects
// on its own only after a connection has once been made, so nothing ever
// tried again. On a busy machine EMQX takes minutes to start after an update,
// so the API came up healthy and stayed deaf to the broker, and nothing
// updated live (messages, typing, presence, notifications) until someone
// restarted it. Seen on a test server, 15 Oct 2026.
//
// Now MqttClient is set before anything is tried, so it is never nil, and the
// first connection is tried with backoff: for bootConnectWait at start-up,
// then in the background until it succeeds. After that the library reconnects
// on any loss, at most connectBackoffMax apart, and onConnect subscribes again
// each time (a clean session keeps no subscriptions). Whether it is connected
// right now is the "realtime" system check, which /health/ready reports.
func ConnectMqtt(mqttConfig *MqttConfig) (err error) {

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"username": mqttConfig.Username,
	})
	jetSecret := os.Getenv("JWT_SECRET")
	tokenString, err := token.SignedString([]byte(jetSecret))

	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"mqttInit/ConnectMqtt Failed to create jwt token err: %+v",
			err,
		)

		return
	}

	client := MQTT.NewClient(clientOptions(mqttConfig, tokenString))
	if helpers.DemoMode() {
		client = demoClient{client}
	}
	MqttClient = client

	timings := currentConnectTimings()
	boot, cancel := context.WithTimeout(context.Background(), bootConnectWait)
	defer cancel()
	if err := connectWithBackoff(boot, client, timings); err == nil {
		return nil
	}
	helpers.MessageLogs.ErrorLog.Printf("mqttInit/ConnectMqtt the broker at %s isn't answering yet: starting without live updates, and connecting in the background", mqttConfig.Broker)
	background := backgroundConnect
	helpers.GoSafeNamed("mqtt:connect", func() { _ = connectWithBackoff(background, client, timings) })
	return nil
}

// clientOptions are the server client's options: who it is, and how it keeps
// its connection once it has one.
func clientOptions(mqttConfig *MqttConfig, password string) *MQTT.ClientOptions {
	opts := MQTT.NewClientOptions()
	opts.AddBroker(mqttConfig.Broker)
	opts.SetClientID(mqttConfig.ClientID)
	opts.SetUsername(mqttConfig.Username)
	opts.SetPassword(password)
	opts.SetCleanSession(mqttConfig.CleanSession)
	opts.SetConnectTimeout(connectAttemptTimeout)
	opts.SetKeepAlive(keepAlive)
	opts.SetPingTimeout(pingTimeout)
	opts.SetAutoReconnect(true)
	opts.SetMaxReconnectInterval(connectBackoffMax)
	opts.SetOnConnectHandler(onConnect)
	opts.SetConnectionLostHandler(func(_ MQTT.Client, err error) {
		helpers.MessageLogs.ErrorLog.Printf("mqttInit lost the broker (%v); reconnecting", err)
	})
	return opts
}

// onConnect runs on the first connection and on every reconnection.
func onConnect(client MQTT.Client) {
	helpers.MessageLogs.InfoLog.Println("Successfully connected mqtt broker !")
	subscribeClientEvents(client)
}

// ConnectWithBackoff makes client's first connection. It tries, and after
// each failure waits helpers.Backoff (connectBackoffBase doubling to
// connectBackoffMax, with jitter) and tries again, until a CONNECT is accepted
// or ctx ends, which bounds the whole wait. It returns as soon as one connect
// succeeds. The caller builds the client, so it chooses the broker and the
// client id: the server through ConnectMqtt, the integration kit with its own.
// Each failure is logged once, with the pause before the next try.
func ConnectWithBackoff(ctx context.Context, client MQTT.Client) error {
	return connectWithBackoff(ctx, client, currentConnectTimings())
}

// connectWithBackoff is ConnectWithBackoff with the timings it was started with.
func connectWithBackoff(ctx context.Context, client MQTT.Client, tm connectTimings) error {
	var err error
	for attempt := 1; ; attempt++ {
		if err = connectOnce(client, tm.attempt); err == nil {
			return nil
		}
		pause := helpers.Backoff(attempt, tm.base, tm.max)
		helpers.MessageLogs.ErrorLog.Printf("mqttInit/ConnectMqtt Failed to connect to mqtt broker (try %d) err: %v; trying again in %s", attempt, err, pause.Round(100*time.Millisecond))
		select {
		case <-ctx.Done():
			return fmt.Errorf("no connection to the broker after %d tries: %w", attempt, err)
		case <-time.After(pause):
		}
	}
}

// connectOnce is one CONNECT, answered or timed out after timeout.
func connectOnce(client MQTT.Client, timeout time.Duration) error {
	tok := client.Connect()
	// The library bounds the attempt itself (SetConnectTimeout); the margin
	// is for a client built without that option.
	if !tok.WaitTimeout(timeout + 5*time.Second) {
		return fmt.Errorf("no answer from the broker within %s", timeout)
	}
	if err := tok.Error(); err != nil {
		return err
	}
	// An attempt made while an earlier one is still in flight is answered as
	// accepted without connecting anything; only the client knows.
	if !client.IsConnected() {
		return errors.New("the broker accepted, but the client isn't connected")
	}
	return nil
}

// ProcessClientID is the MQTT client id this process connects as: the
// configured one, then the process's service role and host. The broker keeps
// one connection per client id and drops the older one when another arrives,
// so go-service and a go-worker (SERVICE_ROLE=worker) both connecting as
// MQTT_CLIENT_ID ("backend") dropped each other over and over, and whichever
// was down could publish nothing.
//
// The role alone wouldn't separate them: workers scale under one role
// (--scale go-worker=3). The host is the container's, the same across
// restarts of that container, so a persistent session (MQTT_CLEAN_SESSION=
// false) is still resumed; every env file uses clean sessions, which keep
// nothing between connections, so the id only has to be unique. With no
// configured id the broker names the client, as before.
func ProcessClientID(configured string, role helpers.ServiceRole) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		var b [4]byte
		_, _ = rand.Read(b[:])
		host = hex.EncodeToString(b[:])
	}
	return clientIDFor(configured, role, host)
}

func clientIDFor(configured string, role helpers.ServiceRole, host string) string {
	if configured == "" {
		return ""
	}
	return configured + "-" + string(role) + "-" + host
}

// The broker's announcements of a client connecting and disconnecting, which
// keep each person's count of connected devices. Shared ($share/backend/):
// with more than one backend process each announcement goes to one of them,
// where a plain subscription sends it to every one, and each would count it.
const (
	clientConnectedTopic    = "$share/backend/$SYS/brokers/+/clients/+/connected"
	clientDisconnectedTopic = "$share/backend/$SYS/brokers/+/clients/+/disconnected"
)

// subscribeClientEvents subscribes to them on every connection, not once at
// boot: a clean session keeps no subscriptions, so after the broker dropped
// this client and it reconnected, the counts stopped.
func subscribeClientEvents(client MQTT.Client) {
	if token := client.Subscribe(clientConnectedTopic, 0, connectMessageHandler); token.Wait() && token.Error() != nil {
		helpers.MessageLogs.ErrorLog.Printf("mqttInit/subscribeClientEvents Failed to subscribe to %s err: %+v", clientConnectedTopic, token.Error())
	} else {
		helpers.MessageLogs.InfoLog.Println("Subscribed to " + clientConnectedTopic)
	}
	if token := client.Subscribe(clientDisconnectedTopic, 0, disconnectMessageHandler); token.Wait() && token.Error() != nil {
		helpers.MessageLogs.ErrorLog.Printf("mqttInit/subscribeClientEvents Failed to subscribe to %s err: %+v", clientDisconnectedTopic, token.Error())
	} else {
		helpers.MessageLogs.InfoLog.Println("Subscribed to " + clientDisconnectedTopic)
	}
}

var connectMessageHandler MQTT.MessageHandler = func(client MQTT.Client, msg MQTT.Message) {

	var mqttClientConnectInfo mqttClientConnect
	err := json.Unmarshal(msg.Payload(), &mqttClientConnectInfo)
	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"mqttInit/connectMessageHandler failed to unmarshal mqtt client connect message json err: %+v",
			err)
		return
	}
	// Only a person's client counts as a person's device. Another backend
	// process ("backend") and the dashboard connect too, and the username was
	// split on "_" and indexed, which panicked, ending the process, on one
	// without it.
	userUUID, ok := helpers.ParseMqttUsername(mqttClientConnectInfo.ClientUserName)
	if !ok {
		return
	}
	userIdString := userUUID.String()

	lock := getUserLock(userIdString)
	lock.Lock()
	defer lock.Unlock()

	ctx := context.Background()

	dgraphUser, err := userDomain.GetDgraphUserInfoByUUID(ctx, userUUID.String())
	if err != nil || dgraphUser == nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"mqttInit/connectMessageHandler failed to get user dgraph userInfo err: %+v",
			err)
		return
	}

	currentCount := 0

	if dgraphUser.DevicesConnected != nil {
		currentCount = *dgraphUser.DevicesConnected
	}

	newCount := currentCount + 1
	dgraphUserUpdated := &dgraphStruct.DgraphUser{
		DevicesConnected: &newCount,
		Uuid:             dgraphUser.Uuid,
	}

	_, err = userDomain.CreateOrUpdateDgraphUser(ctx, dgraphUserUpdated)

	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"mqttInit/connectMessageHandler Failed to increment device connected count err: %+v",
			err)
		return
	}

	mqttCreatePost := mqttStruct.MqttUserDevice{
		Type:     mqttStruct.TYPE_UPDATE,
		Device:   newCount,
		UserUuid: dgraphUser.Uuid,
	}

	go PublishUserDevice(&mqttCreatePost)

}

var disconnectMessageHandler MQTT.MessageHandler = func(client MQTT.Client, msg MQTT.Message) {
	var mqttClientConnectInfo mqttClientConnect
	err := json.Unmarshal(msg.Payload(), &mqttClientConnectInfo)
	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"mqttInit/disconnectMessageHandler failed to unmarshal mqtt client disconnect message json err: %+v",
			err)
		return
	}

	// As on connect: only a person's client is a person's device.
	userUUID, ok := helpers.ParseMqttUsername(mqttClientConnectInfo.ClientUserName)
	if !ok {
		return
	}
	userIdString := userUUID.String()

	lock := getUserLock(userIdString)
	lock.Lock()
	defer lock.Unlock()

	ctx := context.Background()

	dgraphUser, err := userDomain.GetDgraphUserInfoByUUID(ctx, userUUID.String())
	if err != nil || dgraphUser == nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"mqttInit/disconnectMessageHandler failed to get user dgraph userInfo err: %+v",
			err)
		return
	}

	currentCount := 0

	if dgraphUser.DevicesConnected != nil {
		currentCount = *dgraphUser.DevicesConnected
	}

	newCount := currentCount - 1
	if newCount < 0 {
		newCount = 0
	}

	dgraphUserUpdated := &dgraphStruct.DgraphUser{
		DevicesConnected: &newCount,
		Uuid:             userIdString,
	}

	_, err = userDomain.CreateOrUpdateDgraphUser(ctx, dgraphUserUpdated)

	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"mqttInit/disconnectMessageHandler Failed to decrement device connected count err: %+v",
			err)
		return
	}

	mqttCreatePost := mqttStruct.MqttUserDevice{
		Type:     mqttStruct.TYPE_UPDATE,
		Device:   newCount,
		UserUuid: dgraphUser.Uuid,
	}

	go PublishUserDevice(&mqttCreatePost)

}

func PublishUserDevice(mqttUserStatus *mqttStruct.MqttUserDevice) {
	mqttMessaage := mqttStruct.Message{
		Type: mqttStruct.MESSAGE_USER_DEVICE,
		Data: mqttUserStatus,
	}

	marshalMqttUserStatus, err := json.Marshal(mqttMessaage)

	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"mqttInit/PublishUserStatus failed to marshal mqttUserStatus struct err: %+v",
			err)
		return
	}

	mqttClientRes := MqttClient.Publish(helpers.GetPublicUsersStatusTopic(), 0, false, marshalMqttUserStatus)

	go func() {
		_ = mqttClientRes.Wait()
		if mqttClientRes.Error() != nil {
			helpers.MessageLogs.ErrorLog.Printf(
				"mqttInit/PublishUserStatus to publish to mqtt err: %+v",
				mqttClientRes.Error())
			return
		}
	}()

}

// demoClient is the broker client on the public demo. The people a payload
// carries (a forwarded post's author, an activity's actor) come with their
// email address, and one published message reaches every subscriber of its
// topic. So on the demo a payload for a topic the shared visitor may be
// subscribed to goes out with every address but the visitor's own blanked
// (helpers.DemoMqttPayload), as the visitor's HTTP answers are
// (helpers.ServeHidingEmails). That is every shared topic (a channel's or a
// conversation's messages, typing, a doc, a board, a table, everyone's
// status), and so the demo's own members get those blanked too: the broker
// sends the same bytes to each subscriber. One person's activity topic is
// theirs alone (business/MqttAccess refuses anyone else), so another
// person's goes out as it is, and so does the admins' broadcast, which
// MqttAccess opens to admins alone: the visitor is not one.
type demoClient struct{ MQTT.Client }

func (c demoClient) Publish(topic string, qos byte, retained bool, payload interface{}) MQTT.Token {
	if mayReachVisitor(topic) {
		payload = helpers.DemoMqttPayload(payload)
	}
	return c.Client.Publish(topic, qos, retained, payload)
}

// mayReachVisitor reports whether the demo's shared visitor can be among
// topic's subscribers.
func mayReachVisitor(topic string) bool {
	kind, id, ok := helpers.ParseMqttTopic(topic)
	if !ok {
		return true
	}
	switch kind {
	case helpers.MqttKindAdmin:
		return false
	case helpers.MqttKindActivity:
		return isDemoVisitorID(id)
	}
	return true
}

// demoVisitorIDs remembers, by person, whether they are the demo's visitor.
var demoVisitorIDs sync.Map

// emailOf answers a person's address; tests stand in for it.
var emailOf = func(ctx context.Context, id uuid.UUID) (string, error) {
	u, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, id)
	if err != nil {
		return "", err
	}
	if u == nil || u.Id == uuid.Nil {
		return "", errors.New("no such person")
	}
	return u.EmailID, nil
}

// isDemoVisitorID reports whether the person id is the demo's shared
// visitor. Anyone it can't tell counts as the visitor, so their payload is
// blanked, and is asked about again next time.
func isDemoVisitorID(id string) bool {
	if is, ok := demoVisitorIDs.Load(id); ok {
		return is.(bool)
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return true
	}
	email, err := emailOf(context.Background(), parsed)
	if err != nil {
		return true
	}
	is := helpers.IsDemoVisitor(email)
	demoVisitorIDs.Store(id, is)
	return is
}
