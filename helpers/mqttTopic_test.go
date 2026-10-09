package helpers

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base32"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// oldTopic is how every topic was written before mqttTopic: clients hold these
// names (and sessions resume with them), so a topic must not change.
func oldTopic(t *testing.T, prefix, id string) string {
	t.Helper()
	key := sha256.Sum256([]byte("topic-test-secret"))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := make([]byte, aes.BlockSize+len(id))
	copy(ciphertext[:aes.BlockSize], fixedIV)
	cipher.NewCFBEncrypter(block, fixedIV).XORKeyStream(ciphertext[aes.BlockSize:], []byte(id))
	return prefix + regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(base32.StdEncoding.EncodeToString(ciphertext), "")
}

func TestTopicsAreWrittenAsTheyAlwaysWere(t *testing.T) {
	t.Setenv("JWT_SECRET", "topic-test-secret")
	id := uuid.NewString()
	grouping := strings.Repeat("ab", 16) // a conversation's grouping id: 32 hex
	msg, typing := GetMqttTopicForChannel(id)
	dmMsg, dmTyping := GetMqttTopicForDm(grouping)
	for got, want := range map[string]string{
		msg:                                     oldTopic(t, "message/", id),
		typing:                                  oldTopic(t, "typing/", id),
		GetMqttTopicForChannelMessage(id):       oldTopic(t, "message/", id),
		GetMqttTopicForChannelTyping(id):        oldTopic(t, "typing/", id),
		dmMsg:                                   oldTopic(t, "message/", grouping),
		dmTyping:                                oldTopic(t, "typing/", grouping),
		GetMqttTopicForDmMessage(grouping):      oldTopic(t, "message/", grouping),
		GetMqttTopicForDmTyping(grouping):       oldTopic(t, "typing/", grouping),
		GetMqttTopicForProjectMessage(id + "p"): oldTopic(t, "message/", id+"p"),
		GetMqttTopicForUserActivity(id):         oldTopic(t, "activity/", id),
		GetMqttTopicForDoc(id):                  oldTopic(t, "doc/", id),
		GetMqttTopicForBoard(id):                oldTopic(t, "board/", id),
		GetMqttTopicForTable(id):                oldTopic(t, "table/", id),
		GetMqttTopicForAdminBroadcast():         oldTopic(t, "admin/", "admin_broadcast"),
	} {
		if got != want {
			t.Errorf("topic changed: %s, was %s", got, want)
		}
	}
}

func TestATopicReadsBackToWhatItWasWrittenFor(t *testing.T) {
	t.Setenv("JWT_SECRET", "topic-test-secret")
	id := uuid.NewString()
	for _, c := range []struct{ topic, kind, id string }{
		{GetMqttTopicForDoc(id), MqttKindDoc, id},
		{GetMqttTopicForBoard(id), MqttKindBoard, id},
		{GetMqttTopicForTable(id), MqttKindTable, id},
		{GetMqttTopicForChannelMessage(id), MqttKindMessage, id},
		{GetMqttTopicForDmTyping("abc"), MqttKindTyping, "abc"},
		{GetMqttTopicForUserActivity(id), MqttKindActivity, id},
		{GetMqttTopicForAdminBroadcast(), MqttKindAdmin, "admin_broadcast"},
	} {
		kind, got, ok := ParseMqttTopic(c.topic)
		if !ok || kind != c.kind || got != c.id {
			t.Errorf("%s read back as %q %q %v", c.topic, kind, got, ok)
		}
	}

	doc := GetMqttTopicForDoc(id)
	_, encoded, _ := strings.Cut(doc, "/")
	t.Setenv("JWT_SECRET", "topic-test-secret")
	for name, topic := range map[string]string{
		"a wildcard":                 "message/#",
		"a wildcard level":           "doc/+",
		"everything":                 "#",
		"a shared subscription":      "$share/g/" + doc,
		"a system topic":             "$SYS/brokers",
		"an unknown kind":            "secret/" + encoded,
		"no id":                      "doc/",
		"no kind":                    encoded,
		"lower case":                 "doc/" + strings.ToLower(encoded),
		"padded":                     doc + "====",
		"another level":              doc + "/x",
		"cut short":                  doc[:len(doc)-3],
		"only the IV":                "doc/" + topicEncoding.EncodeToString(fixedIV),
		"public status as encrypted": "public/" + encoded,
		"a huge topic":               "doc/" + strings.Repeat("A", 4096),
	} {
		if kind, got, ok := ParseMqttTopic(topic); ok {
			t.Errorf("%s was read as %q %q", name, kind, got)
		}
	}

	// Written under another server's key, it reads back as nothing of ours.
	t.Setenv("JWT_SECRET", "another-server")
	foreign := GetMqttTopicForDoc(id)
	t.Setenv("JWT_SECRET", "topic-test-secret")
	if _, got, ok := ParseMqttTopic(foreign); ok && got == id {
		t.Error("another server's topic read back as the same doc")
	}
}
