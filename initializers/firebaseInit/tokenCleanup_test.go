package firebaseInit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	firebase "firebase.google.com/go/v4"
	"google.golang.org/api/option"

	"github.com/akashc777/OneCamp/helpers"
)

// A push against a fake FCM, through the real SDK. Every token was deleted on
// any error, and message bodies went in whole, which FCM refuses over 4096
// bytes: one long paste into a channel unregistered every member's devices
// until each reopened the app, and an FCM outage did the same (TN-BUG-14).
// Now the body is cut so the push delivers, and only a token FCM says is gone
// is deleted.
func TestAPushKeepsTokensUnlessFCMSaysTheyAreGone(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	delivered := map[string]map[string]string{}
	fcm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Message struct {
				Token   string `json:"token"`
				Webpush struct {
					Data map[string]string `json:"data"`
				} `json:"webpush"`
			} `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("the push isn't JSON: %v", err)
		}
		size := 0
		for k, v := range req.Message.Webpush.Data {
			size += len(k) + len(v)
		}
		refuse := func(status int, state, msg, fcmCode string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":{"code":%d,"message":%q,"status":%q,"details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":%q}]}}`, status, msg, state, fcmCode)
		}
		switch token := req.Message.Token; {
		case size > 4096:
			refuse(400, "INVALID_ARGUMENT", "Message is too big", "INVALID_ARGUMENT")
		case token == "uninstalled":
			refuse(404, "NOT_FOUND", "Requested entity was not found.", "UNREGISTERED")
		case token == "other-project":
			refuse(403, "PERMISSION_DENIED", "SenderId mismatch", "SENDER_ID_MISMATCH")
		case token == "garbled":
			refuse(400, "INVALID_ARGUMENT", "The registration token is not a valid FCM registration token", "INVALID_ARGUMENT")
		case token == "fcm-down":
			refuse(500, "INTERNAL", "Internal error encountered.", "INTERNAL")
		default:
			mu.Lock()
			delivered[token] = req.Message.Webpush.Data
			mu.Unlock()
			fmt.Fprint(w, `{"name":"projects/test/messages/1"}`)
		}
	}))
	defer fcm.Close()

	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: "test"}, option.WithEndpoint(fcm.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	client, err := app.Messaging(ctx)
	if err != nil {
		t.Fatal(err)
	}
	saved := FirebaseApp.Messaging()
	FirebaseApp.setMessaging(client)
	t.Cleanup(func() { FirebaseApp.setMessaging(saved) })
	var dropped []string
	savedDrop := dropTokens
	dropTokens = func(_ context.Context, tokens []string) error {
		mu.Lock()
		dropped = append(dropped, tokens...)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { dropTokens = savedDrop })

	paste := strings.Repeat("panic: runtime error: index out of range [3] with length 3\n", 80) // ~4.7 KB
	tokens := []string{"maya-phone", "uninstalled", "other-project", "garbled", "fcm-down", "jonas-laptop"}
	if err := FirebaseApp.MultiCastPush(ctx, map[string]string{
		FIREBASE_PUSH_DATA_TYPE: FIREBASE_PUSH_DATA_TYPE_CHANNEL, FIREBASE_PUSH_DATA_TITLE: "#engineering",
		FIREBASE_PUSH_DATA_BODY: paste,
	}, tokens); err != nil {
		t.Fatal(err)
	}

	for _, who := range []string{"maya-phone", "jonas-laptop"} {
		body, ok := delivered[who][FIREBASE_PUSH_DATA_BODY]
		if !ok {
			t.Fatalf("%s got no push: a long message must still go out", who)
		}
		if len(body) > helpers.PushBodyMax || !strings.HasSuffix(body, "…") {
			t.Fatalf("%s got a body of %d bytes", who, len(body))
		}
	}
	slices.Sort(dropped)
	if want := []string{"garbled", "other-project", "uninstalled"}; !slices.Equal(dropped, want) {
		t.Fatalf("dropped %v, want only the tokens FCM says are gone %v (an FCM fault keeps its token)", dropped, want)
	}
}
