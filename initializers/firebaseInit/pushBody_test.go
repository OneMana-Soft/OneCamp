package firebaseInit

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/akashc777/OneCamp/helpers"
)

func dataSize(data map[string]string) int {
	n := 0
	for k, v := range data {
		n += len(k) + len(v)
	}
	return n
}

// A push's body is cut so the whole data payload stays under what FCM takes:
// a 4 KB paste went in whole and was refused for every device (TN-BUG-14).
func TestFitPushBody(t *testing.T) {
	paste := strings.Repeat("connection reset by peer; ", 200) // ~5 KB
	data := map[string]string{
		FIREBASE_PUSH_DATA_TYPE: FIREBASE_PUSH_DATA_TYPE_CHANNEL, FIREBASE_PUSH_DATA_TYPE_ID: "c0ffee00-0000-4000-8000-000000000001",
		FIREBASE_PUSH_DATA_TITLE: "#engineering", FIREBASE_PUSH_DATA_BODY: paste, FIREBASE_PUSH_DATA_ICON: "/icons/icon-circle-512.png",
	}
	fitPushBody(data)
	if body := data[FIREBASE_PUSH_DATA_BODY]; len(body) > helpers.PushBodyMax || !strings.HasSuffix(body, "…") {
		t.Fatalf("a 5 KB body went out as %d bytes", len(body))
	}
	again := data[FIREBASE_PUSH_DATA_BODY]
	if fitPushBody(data); data[FIREBASE_PUSH_DATA_BODY] != again {
		t.Fatal("cutting twice changed the body: each batch of tokens must get the same push")
	}

	// A long signed icon address leaves less room: the body gives way.
	data[FIREBASE_PUSH_DATA_BODY] = paste
	data[FIREBASE_PUSH_DATA_ICON] = "https://files.example.test/avatar.png?" + strings.Repeat("X-Amz-Signature=abcdef0123456789&", 95)
	fitPushBody(data)
	if size := dataSize(data); size > fcmDataMax || !utf8.ValidString(data[FIREBASE_PUSH_DATA_BODY]) {
		t.Fatalf("the payload is %d bytes, over %d", size, fcmDataMax)
	}

	// No body, nothing added.
	none := map[string]string{FIREBASE_PUSH_DATA_TYPE: FIREBASE_PUSH_DATA_TYPE_TASK}
	if fitPushBody(none); len(none) != 1 {
		t.Fatalf("a push with no body got one: %v", none)
	}
}
