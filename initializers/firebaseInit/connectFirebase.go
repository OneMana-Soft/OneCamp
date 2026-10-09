package firebaseInit

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync/atomic"
	"time"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/messaging"
	userFCMtokenBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	"github.com/akashc777/OneCamp/helpers"
	"google.golang.org/api/option"
	"strings"
	"sync"
)

type firebaseAppStruct struct {
	// Guarded because the credential can now be replaced from the admin screen
	// while requests are in flight. Before that this was written once at boot
	// and only ever read, so a bare field was correct; it is not any more.
	mu              sync.RWMutex
	messagingClient *messaging.Client
}

// Messaging returns the current client, or nil when push is not configured.
func (f *firebaseAppStruct) Messaging() *messaging.Client {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.messagingClient
}

// setMessaging swaps the client in. Only ever called with a client that has
// already been built successfully, so a rejected credential leaves the working
// one in place rather than turning push off to find out.
func (f *firebaseAppStruct) setMessaging(c *messaging.Client) {
	f.mu.Lock()
	f.messagingClient = c
	f.mu.Unlock()
}

type FirebaseAppConfigStruct struct {
	// FirebaseCredJSON is the service-account document itself.
	//
	// The document rather than a path, because the credential now comes from an
	// admin setting as often as from a mounted file, and resolving which is the
	// caller's job. Push stays optional: empty means not configured, which is a
	// supported state and not an error.
	FirebaseCredJSON string
}

const (
	FIREBASE_PUSH_DATA_TYPE                       = "type"
	FIREBASE_PUSH_DATA_TYPE_ID                    = "type_id"
	FIREBASE_PUSH_DATA_THREAD_ID                  = "thread_id"
	FIREBASE_PUSH_DATA_TYPE_CHANNEL               = "channel"
	FIREBASE_PUSH_DATA_TYPE_CHANNEL_CALL          = "channel_call"
	FIREBASE_PUSH_DATA_TYPE_TASK                  = "task"
	FIREBASE_PUSH_DATA_TYPE_CHAT                  = "chat"
	FIREBASE_PUSH_DATA_TYPE_CHAT_CALL             = "chat_call"
	FIREBASE_PUSH_DATA_TYPE_CHAT_REACTION         = "chat_reaction"
	FIREBASE_PUSH_DATA_TYPE_CHAT_COMMENT_REACTION = "chat_comment_reaction"
	FIREBASE_PUSH_DATA_TYPE_CHAT_COMMENT          = "chat_comment"
	FIREBASE_PUSH_DATA_TYPE_POST_COMMENT          = "post_comment"
	FIREBASE_PUSH_DATA_TYPE_TASK_COMMENT          = "task_comment"
	FIREBASE_PUSH_DATA_TYPE_DOC_COMMENT           = "doc_comment"
	FIREBASE_PUSH_DATA_TYPE_REMINDER              = "reminder"
	FIREBASE_PUSH_DATA_TYPE_LATER                 = "later"
	// An agent is waiting for a decision; the click opens Home, where
	// "Needs your approval" leads the list.
	FIREBASE_PUSH_DATA_TYPE_APPROVAL = "approval"
	// The notification tag; one per approval so two do not replace each other.
	FIREBASE_PUSH_DATA_TAG         = "tag"
	FIREBASE_PUSH_DATA_TITLE       = "title"
	FIREBASE_PUSH_DATA_BODY        = "body"
	FIREBASE_PUSH_DATA_USERNAME    = "username"
	FIREBASE_PUSH_DATA_REACTION_ID = "reaction_id"
	FIREBASE_PUSH_DATA_ICON        = "icon"
)

var FirebaseApp firebaseAppStruct

func ConnectFirebase(config *FirebaseAppConfigStruct) (err error) {

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if strings.TrimSpace(config.FirebaseCredJSON) == "" {
		// Not configured is not a failure. Every notification path already
		// tolerates a nil client, and saying this plainly at boot is the
		// difference between "push is off" and an error an operator chases.
		// Through the context logger, not MessageLogs: that pointer is nil until
		// InitLogger runs, so logging here would panic on the most common
		// configuration there is, which is not having push set up at all.
		helpers.LogInfoWithContext(ctx, "firebase: no credential configured, push notifications are off")
		FirebaseApp.setMessaging(nil)
		return nil
	}

	opt := option.WithCredentialsJSON([]byte(config.FirebaseCredJSON))
	app, err := firebase.NewApp(ctx, nil, opt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "firebase: credential rejected: %+v", err)
		return err
	}

	client, err := app.Messaging(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "firebase: could not build the messaging client: %+v", err)
		return err
	}

	// Swapped only now. A bad credential leaves whatever was working in place.
	FirebaseApp.setMessaging(client)
	helpers.LogInfoWithContext(ctx, "firebase: push notifications are on")
	return nil
}

// pushObserver sees every push MultiCastPush is asked for, sent or not. Only
// tests set it (ObservePushesForTest).
var pushObserver atomic.Pointer[func(data map[string]string, tokens []string)]

// ObservePushesForTest shows fn every push MultiCastPush is asked for, whether
// or not push is configured, so an integration test can see whom a path
// notifies without a Firebase project. restore stops it.
func ObservePushesForTest(fn func(data map[string]string, tokens []string)) (restore func()) {
	pushObserver.Store(&fn)
	return func() { pushObserver.Store(nil) }
}

func (f *firebaseAppStruct) MultiCastPush(ctx context.Context, pushData map[string]string, tokens []string) (err error) {
	if observe := pushObserver.Load(); observe != nil {
		(*observe)(maps.Clone(pushData), slices.Clone(tokens))
	}
	// Push is optional. A self-hosted install may have no Firebase credentials, in
	// which case MessagingClient is nil and every notification path still reaches
	// here. Returning quietly is the whole point of treating push as optional:
	// losing it must cost only push, not a panic on the next chat message.
	client := FirebaseApp.Messaging()
	if client == nil {
		return nil
	}

	// token limit is 500

	// Default the notification icon into the payload the clients actually read.
	// This used to compute a local `icon` variable that was never written back
	// anywhere, so the default was dead code — and it named icon-192x192.png,
	// which does not exist in the frontend's public/icons either. Both clients
	// happen to apply this same fallback themselves, which is why nothing looked
	// broken; setting it here makes the payload self-describing for any client
	// that doesn't. Idempotent, so re-running per token batch is safe.
	if pushData[FIREBASE_PUSH_DATA_ICON] == "" {
		pushData[FIREBASE_PUSH_DATA_ICON] = "/icons/icon-circle-512.png"
	}

	message := &messaging.MulticastMessage{
		Tokens: tokens,
		Webpush: &messaging.WebpushConfig{
			Headers: map[string]string{
				"TTL": "4500",
			},
			Data: pushData,
			FCMOptions: &messaging.WebpushFCMOptions{
				Link: fmt.Sprintf("https://%s/", os.Getenv("FE_DOMAIN")),
			},
		},
		Android: &messaging.AndroidConfig{
			Data: pushData,
		},
	}

	response, err := client.SendEachForMulticast(ctx, message)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"firebaseInit/UpdateChannelNotificationType failed sending muticast message err: %+v",
			err)
		return
	}

	removeTokenList := []string{}
	for ind, resp := range response.Responses {
		if resp.Error != nil || messaging.IsUnregistered(resp.Error) || messaging.IsInvalidArgument(resp.Error) {
			removeTokenList = append(removeTokenList, tokens[ind])
		}
	}

	if len(removeTokenList) > 0 {
		err = userFCMtokenBusiness.DeleteFromListOfFCMToken(ctx, removeTokenList)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"firebaseInit/UpdateChannelNotificationType failed delete fcm tokens err: %+v",
				err)
			return
		}
	}
	return
}
