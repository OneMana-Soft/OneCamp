package mqttInit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/golang-jwt/jwt/v5"
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

	opts := MQTT.NewClientOptions()

	opts.AddBroker(mqttConfig.Broker)
	opts.SetClientID(mqttConfig.ClientID)
	opts.SetUsername(mqttConfig.Username)
	opts.SetPassword(tokenString)
	opts.SetCleanSession(mqttConfig.CleanSession)
	opts.SetOnConnectHandler(subscribeClientEvents)

	MqttClient = MQTT.NewClient(opts)
	if token := MqttClient.Connect(); token.Wait() && token.Error() != nil {
		helpers.MessageLogs.ErrorLog.Printf("mqttInit/ConnectMqtt Failed to connect to mqtt broker err: %+v", token.Error())
		return
	}
	helpers.MessageLogs.InfoLog.Println("Successfully connected mqtt broker !")

	return
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
