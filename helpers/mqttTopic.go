package helpers

// MQTT topics.
//
// A topic is a kind and an id: "doc/" and a doc's id, "message/" and a
// channel's, a conversation's or a project's. The id is encrypted under a key
// derived from JWT_SECRET (AES-CFB with a fixed IV, so a thing's topic is the
// same every time) and written in base32 without padding.
//
// The encryption hides which thing a topic is for. It is not what keeps a
// person out of one: the broker asks the backend before every subscription
// (business/MqttAccess), and the backend reads the id back out of the topic
// (ParseMqttTopic) to check that the person may read that thing. Before that
// check existed, a subscription to "message/#" received every channel's and
// every conversation's messages.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base32"
	"os"
	"strings"
)

// The kinds of topic, each the first part of its topics.
const (
	MqttKindMessage  = "message"  // a channel's, a conversation's or a project's messages
	MqttKindTyping   = "typing"   // who is typing in a channel or a conversation
	MqttKindActivity = "activity" // one person's notifications
	MqttKindDoc      = "doc"
	MqttKindBoard    = "board"
	MqttKindTable    = "table"
	MqttKindAdmin    = "admin" // the system admins' broadcast
)

var mqttKinds = map[string]bool{
	MqttKindMessage: true, MqttKindTyping: true, MqttKindActivity: true,
	MqttKindDoc: true, MqttKindBoard: true, MqttKindTable: true, MqttKindAdmin: true,
}

// fixedIV keeps a thing's topic the same every time it's written. It is also
// the first block of every encrypted topic.
var fixedIV = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

// topicEncoding is base32 without its padding, which topics have never had.
var topicEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// maxTopicLen bounds what ParseMqttTopic decodes. The longest id written into a
// topic is a uuid; a topic many times that long is nothing this server wrote.
const maxTopicLen = 256

func mqttTopicCipher() (cipher.Block, error) {
	key := sha256.Sum256([]byte(os.Getenv("JWT_SECRET")))
	return aes.NewCipher(key[:])
}

// mqttTopic writes the topic of one thing.
func mqttTopic(kind, id string) string {
	block, err := mqttTopicCipher()
	if err != nil {
		// A 32-byte key is always accepted; this can't happen.
		MessageLogs.ErrorLog.Printf("helpers/mqttTopic Failed to make the cipher err: %+v", err)
		return ""
	}
	ciphertext := make([]byte, aes.BlockSize+len(id))
	copy(ciphertext, fixedIV)
	cipher.NewCFBEncrypter(block, fixedIV).XORKeyStream(ciphertext[aes.BlockSize:], []byte(id))
	return kind + "/" + topicEncoding.EncodeToString(ciphertext)
}

// ParseMqttTopic reads a topic back into its kind and the id it was written
// for. ok is false for anything this server didn't write: an unknown kind, a
// wildcard, an encoding that doesn't decode, or one that decodes but isn't
// written exactly as mqttTopic writes it.
func ParseMqttTopic(topic string) (kind, id string, ok bool) {
	if len(topic) > maxTopicLen {
		return "", "", false
	}
	kind, encoded, found := strings.Cut(topic, "/")
	if !found || !mqttKinds[kind] {
		return "", "", false
	}
	raw, err := topicEncoding.DecodeString(encoded)
	if err != nil || len(raw) <= aes.BlockSize || !bytes.Equal(raw[:aes.BlockSize], fixedIV) {
		return "", "", false
	}
	block, err := mqttTopicCipher()
	if err != nil {
		return "", "", false
	}
	plain := make([]byte, len(raw)-aes.BlockSize)
	cipher.NewCFBDecrypter(block, fixedIV).XORKeyStream(plain, raw[aes.BlockSize:])
	id = string(plain)
	// Only the one way of writing it: the same id written any other way is a
	// topic nothing is ever published to.
	if mqttTopic(kind, id) != topic {
		return "", "", false
	}
	return kind, id, true
}

func GetMqttTopicForDm(groupingId string) (messageTopicName string, typingTopicName string) {
	return mqttTopic(MqttKindMessage, groupingId), mqttTopic(MqttKindTyping, groupingId)
}

func GetMqttTopicForDmMessage(groupingId string) (topicName string) {
	return mqttTopic(MqttKindMessage, groupingId)
}

func GetMqttTopicForDmTyping(groupingId string) (topicName string) {
	return mqttTopic(MqttKindTyping, groupingId)
}

func GetMqttTopicForChannel(channelId string) (messageTopicName string, typingTopicName string) {
	return mqttTopic(MqttKindMessage, channelId), mqttTopic(MqttKindTyping, channelId)
}

func GetMqttTopicForChannelMessage(channelId string) (topicName string) {
	return mqttTopic(MqttKindMessage, channelId)
}

func GetMqttTopicForChannelTyping(channelId string) (topicName string) {
	return mqttTopic(MqttKindTyping, channelId)
}

func GetMqttTopicForProjectMessage(projectId string) (topicName string) {
	return mqttTopic(MqttKindMessage, projectId)
}

func GetMqttTopicForUserActivity(userUUID string) (topicName string) {
	return mqttTopic(MqttKindActivity, userUUID)
}

func GetMqttTopicForDoc(docId string) (messageTopicName string) {
	return mqttTopic(MqttKindDoc, docId)
}

// GetMqttTopicForBoard returns the per-board MQTT topic (board comments /
// presence), mirroring GetMqttTopicForDoc. Real-time canvas sync itself runs
// over the Hocuspocus/Yjs collaboration service, not MQTT.
func GetMqttTopicForBoard(boardId string) (messageTopicName string) {
	return mqttTopic(MqttKindBoard, boardId)
}

// GetMqttTopicForTable returns the per-table MQTT topic used to broadcast row
// create/update/delete events to open grid/board/calendar views, mirroring
// GetMqttTopicForDoc.
func GetMqttTopicForTable(tableId string) (messageTopicName string) {
	return mqttTopic(MqttKindTable, tableId)
}

// GetMqttTopicForAdminBroadcast returns the topic for system-wide admin
// notifications (archive job status, future admin events). Only system admins
// are handed it, and only they may subscribe to it.
func GetMqttTopicForAdminBroadcast() (topicName string) {
	return mqttTopic(MqttKindAdmin, "admin_broadcast")
}

// GetPublicUsersStatusTopic is the one topic everyone signed in reads: who is
// online. It isn't encrypted.
func GetPublicUsersStatusTopic() (topicName string) {
	return "public/userStatus"
}
