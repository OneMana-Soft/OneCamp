package mqttInit

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"

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

	MqttClient = MQTT.NewClient(opts)
	if token := MqttClient.Connect(); token.Wait() && token.Error() != nil {
		helpers.MessageLogs.ErrorLog.Printf("mqttInit/ConnectMqtt Failed to connect to mqtt broker err: %+v", token.Error())
		return
	}
	helpers.MessageLogs.InfoLog.Println("Successfully connected mqtt broker !")

	if token := MqttClient.Subscribe("$SYS/brokers/+/clients/+/connected", 0, connectMessageHandler); token.Wait() && token.Error() != nil {
		helpers.MessageLogs.ErrorLog.Printf("mqttInit/ConnectMqtt Failed to connect to mqtt broker err: %+v", token.Error())
	}
	helpers.MessageLogs.InfoLog.Println("Subscribed to $SYS/brokers/+/clients/+/connected")

	if token := MqttClient.Subscribe("$SYS/brokers/+/clients/+/disconnected", 0, disconnectMessageHandler); token.Wait() && token.Error() != nil {
		helpers.MessageLogs.ErrorLog.Printf("mqttInit/ConnectMqtt Failed to connect to mqtt broker err: %+v", token.Error())
	}
	helpers.MessageLogs.InfoLog.Println("Subscribed to $SYS/brokers/+/clients/+/disconnected")

	return
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
	userIdString := strings.Split(mqttClientConnectInfo.ClientUserName, "_")[1]

	lock := getUserLock(userIdString)
	lock.Lock()
	defer lock.Unlock()

	ctx := context.Background()

	userUUID, err := uuid.Parse(userIdString)
	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"mqttInit/connectMessageHandler Failed to parse string to uuid err: %+v",
			err)
		return
	}

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

	userIdString := strings.Split(mqttClientConnectInfo.ClientUserName, "_")[1]

	lock := getUserLock(userIdString)
	lock.Lock()
	defer lock.Unlock()

	ctx := context.Background()
	userUUID, err := uuid.Parse(userIdString)
	if err != nil {
		helpers.MessageLogs.ErrorLog.Printf(
			"mqttInit/disconnectMessageHandler Failed to parse string to uuid err: %+v",
			err)
		return
	}

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
