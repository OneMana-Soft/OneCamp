package business

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/mqttInit"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// Every Publish* goes out through publishToTopic (D1): one place marshals the
// message, sends it and waits for the broker, so no publisher can drift from
// the others, and none panics when no broker is connected (a test, a tool, a
// one-shot command): MqttClient is an interface, and a nil one used to panic
// in 18 of them.

// sentMessage is one publish a recordingClient saw.
type sentMessage struct {
	topic    string
	qos      byte
	retained bool
	payload  []byte
}

// doneToken is a publish the broker has answered.
type doneToken struct{ MQTT.Token }

func (doneToken) Wait() bool                     { return true }
func (doneToken) WaitTimeout(time.Duration) bool { return true }
func (doneToken) Error() error                   { return nil }
func (doneToken) Done() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// recordingClient is a connected broker that keeps what it is sent.
type recordingClient struct {
	MQTT.Client
	mu   sync.Mutex
	sent []sentMessage
}

func (c *recordingClient) IsConnected() bool { return true }
func (c *recordingClient) Publish(topic string, qos byte, retained bool, payload interface{}) MQTT.Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, sentMessage{topic, qos, retained, payload.([]byte)})
	return doneToken{}
}

// publisher is one Publish* with what it must send: its topic, its QoS and
// its message (a kind and the data it carries).
type publisher struct {
	name    string
	publish func()
	topics  []string
	qos     byte
	kind    int8
	data    any
}

func publishers() []publisher {
	post := &mqttStruct.MqttPost{PostUuid: "p-1", PostHtmlText: "<p>hi</p>"}
	channelCall := &mqttStruct.MqttChannelCall{}
	chatCall := &mqttStruct.MqttChatCall{}
	postReaction := &mqttStruct.MqttPostReaction{}
	docCommentReaction := &mqttStruct.MqttDocCommentReaction{}
	postCommentReaction := &mqttStruct.MqttPostCommentReaction{}
	channelTyping := &mqttStruct.MqttChannelTyping{}
	docComment := &mqttStruct.MqttDocComment{}
	postComment := &mqttStruct.MqttPostComment{}
	chat := &mqttStruct.MqttChat{}
	chatReaction := &mqttStruct.MqttChatReaction{}
	chatCommentReaction := &mqttStruct.MqttChatCommentReaction{}
	chatTyping := &mqttStruct.MqttChatTyping{}
	emoji := &mqttStruct.MqttUserEmojiStatus{}
	status := &mqttStruct.MqttUserStatus{}
	chatComment := &mqttStruct.MqttChatComment{}
	activity := &mqttStruct.MqttActivity{}
	archive := &mqttStruct.MqttArchiveJobStatus{}
	progress := &mqttStruct.MqttSlackImportProgress{ErrorMessage: "failed at https://files.example/x?token=secret"}
	sentProgress := *progress
	sentProgress.ErrorMessage = helpers.WithoutURLQueries(progress.ErrorMessage)
	row := map[string]any{"row": "r-1"}
	ch, grp, doc, user, table, board := "ch-1", "grp-1", "doc-1", "user-1", "table-1", "board-1"
	return []publisher{
		{"PublishPost", func() { PublishPost(post, ch) }, []string{helpers.GetMqttTopicForChannelMessage(ch)}, 1, mqttStruct.MESSAGE_POST, post},
		{"PublishChannelUpdate", func() { PublishChannelUpdate(ch, "archived") }, []string{helpers.GetMqttTopicForChannelMessage(ch)}, 1, mqttStruct.MESSAGE_CHANNEL_UPDATE,
			&mqttStruct.MqttChannelUpdate{Type: mqttStruct.TYPE_UPDATE, ChannelUUID: ch, Action: "archived"}},
		{"PublishChannelCall", func() { PublishChannelCall(channelCall, ch) }, []string{helpers.GetMqttTopicForChannelMessage(ch)}, 1, mqttStruct.MESSAGE_CHANNEL_CALL, channelCall},
		{"PublishChatCall", func() { PublishChatCall(chatCall, grp) }, []string{helpers.GetMqttTopicForDmMessage(grp)}, 1, mqttStruct.MESSAGE_CHAT_CALL, chatCall},
		{"PublishPostReaction", func() { PublishPostReaction(postReaction, ch) }, []string{helpers.GetMqttTopicForChannelMessage(ch)}, 1, mqttStruct.MESSAGE_POST_REACTION, postReaction},
		{"PublishDocCommentReaction", func() { PublishDocCommentReaction(docCommentReaction, doc) }, []string{helpers.GetMqttTopicForDoc(doc)}, 1, mqttStruct.MESSAGE_DOC_COMMENT_REACTION, docCommentReaction},
		{"PublishPostCommentReaction", func() { PublishPostCommentReaction(postCommentReaction, ch) }, []string{helpers.GetMqttTopicForChannelMessage(ch)}, 1, mqttStruct.MESSAGE_POST_COMMENT_REACTION, postCommentReaction},
		{"PublishChannelTyping", func() { PublishChannelTyping(channelTyping, ch) }, []string{helpers.GetMqttTopicForChannelTyping(ch)}, 0, mqttStruct.MESSAGE_POST_TYPING, channelTyping},
		{"PublishDocComment", func() { PublishDocComment(docComment, doc) }, []string{helpers.GetMqttTopicForDoc(doc)}, 1, mqttStruct.MESSAGE_DOC_COMMENT, docComment},
		{"PublishPostComment", func() { PublishPostComment(postComment, ch) }, []string{helpers.GetMqttTopicForChannelMessage(ch)}, 1, mqttStruct.MESSAGE_POST_COMMENT, postComment},
		{"PublishChat", func() { PublishChat(chat, grp) }, []string{helpers.GetMqttTopicForDmMessage(grp)}, 1, mqttStruct.MESSAGE_CHAT, chat},
		{"PublishChatReaction", func() { PublishChatReaction(chatReaction, grp) }, []string{helpers.GetMqttTopicForDmMessage(grp)}, 1, mqttStruct.MESSAGE_CHAT_REACTION, chatReaction},
		{"PublishChatCommentReaction", func() { PublishChatCommentReaction(chatCommentReaction, grp) }, []string{helpers.GetMqttTopicForDmMessage(grp)}, 1, mqttStruct.MESSAGE_CHAT_COMMENT_REACTION, chatCommentReaction},
		{"PublishChatTyping", func() { PublishChatTyping(chatTyping, grp) }, []string{helpers.GetMqttTopicForDmTyping(grp)}, 0, mqttStruct.MESSAGE_CHAT_TYPING, chatTyping},
		{"PublishUserEmojiStatus", func() { PublishUserEmojiStatus(emoji) }, []string{helpers.GetPublicUsersStatusTopic()}, 1, mqttStruct.MESSAGE_USER_EMOJI_STATUS, emoji},
		{"PublishUserStatus", func() { PublishUserStatus(status) }, []string{helpers.GetPublicUsersStatusTopic()}, 1, mqttStruct.MESSAGE_USER_STATUS, status},
		{"PublishChatComment", func() { PublishChatComment(chatComment, grp) }, []string{helpers.GetMqttTopicForDmMessage(grp)}, 1, mqttStruct.MESSAGE_CHAT_COMMENT, chatComment},
		{"PublishActivity", func() { PublishActivity(activity, user) }, []string{helpers.GetMqttTopicForUserActivity(user)}, 1, mqttStruct.MESSAGE_ACTIVITY, activity},
		{"PublishMessageToUser", func() { PublishMessageToUser(user, mqttStruct.MESSAGE_ACTIVITY, row) }, []string{helpers.GetMqttTopicForUserActivity(user)}, 1, mqttStruct.MESSAGE_ACTIVITY, row},
		{"PublishToTopics", func() { PublishToTopics([]string{"t/a", "", "t/b", "t/a"}, mqttStruct.MESSAGE_ACTIVITY, row) }, []string{"t/a", "t/b"}, 1, mqttStruct.MESSAGE_ACTIVITY, row},
		{"PublishConversationAccess", func() { PublishConversationAccess("dm", grp, "removed", user) }, []string{helpers.GetMqttTopicForUserActivity(user)}, 1, mqttStruct.MESSAGE_CONVERSATION_ACCESS,
			&mqttStruct.MqttConversationAccess{Kind: "dm", Id: grp, Change: "removed"}},
		{"PublishArchiveJobStatus", func() { PublishArchiveJobStatus(archive) }, []string{helpers.GetMqttTopicForAdminBroadcast()}, 1, mqttStruct.MESSAGE_ARCHIVE_JOB_STATUS, archive},
		{"PublishSlackImportProgress", func() { PublishSlackImportProgress(progress) }, []string{helpers.GetMqttTopicForAdminBroadcast()}, 1, mqttStruct.MESSAGE_SLACK_IMPORT_PROGRESS, &sentProgress},
		{"PublishTableRow", func() { PublishTableRow(table, row) }, []string{helpers.GetMqttTopicForTable(table)}, 1, mqttStruct.MESSAGE_TABLE_ROW, row},
		{"PublishTablePresence", func() { PublishTablePresence(table, row) }, []string{helpers.GetMqttTopicForTablePresence(table)}, 0, mqttStruct.MESSAGE_TABLE_ROW, row},
		{"PublishPollUpdate", func() { PublishPollUpdate(ch, "poll-1") }, []string{helpers.GetMqttTopicForChannelMessage(ch)}, 1, mqttStruct.MESSAGE_POLL_UPDATE,
			&mqttStruct.MqttPollUpdate{Type: mqttStruct.TYPE_UPDATE, PollUUID: "poll-1", ChannelUUID: ch}},
		{"PublishBoardPollUpdate", func() { PublishBoardPollUpdate(board, "poll-1", "frame-1", mqttStruct.TYPE_UPDATE) }, []string{helpers.GetMqttTopicForBoard(board)}, 1, mqttStruct.MESSAGE_POLL_UPDATE,
			&mqttStruct.MqttPollUpdate{Type: mqttStruct.TYPE_UPDATE, PollUUID: "poll-1", BoardUUID: board, FrameID: "frame-1"}},
	}
}

// With no broker connected, no publisher panics: each sends nothing.
func TestNoPublisherPanicsWithoutABroker(t *testing.T) {
	live := mqttInit.MqttClient
	t.Cleanup(func() { mqttInit.MqttClient = live })
	mqttInit.MqttClient = nil
	for _, p := range publishers() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s panics with no broker connected: %v", p.name, r)
				}
			}()
			p.publish()
		}()
	}
}

// With a broker, each publisher sends what it always has: its topic, its QoS,
// never retained, and its message as {type, data}.
func TestEveryPublisherSendsItsTopicKindAndPayload(t *testing.T) {
	live := mqttInit.MqttClient
	t.Cleanup(func() { mqttInit.MqttClient = live })
	for _, p := range publishers() {
		c := &recordingClient{}
		mqttInit.MqttClient = c
		p.publish()
		want, err := json.Marshal(mqttStruct.Message{Type: p.kind, Data: p.data})
		if err != nil {
			t.Fatal(err)
		}
		if len(c.sent) != len(p.topics) {
			t.Errorf("%s sent %d messages, want %d", p.name, len(c.sent), len(p.topics))
			continue
		}
		for i, s := range c.sent {
			if s.topic != p.topics[i] || s.qos != p.qos || s.retained || string(s.payload) != string(want) {
				t.Errorf("%s sent %q qos %d retained %v %s, want %q qos %d %s", p.name, s.topic, s.qos, s.retained, s.payload, p.topics[i], p.qos, want)
			}
		}
	}
}
