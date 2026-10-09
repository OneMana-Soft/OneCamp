// Package business (MqttAccess) answers the broker: may this person's client
// subscribe to this topic?
//
// The broker asks before every subscription and every publish a person's
// client makes (emqx's HTTP authorization source, POST
// /internal/mqtt/authorize), and nothing gets through without a yes from here.
// It used to decide alone, from a list of topic prefixes every signed-in person
// could subscribe under: a subscription to "message/#" received every
// channel's and every conversation's messages, private ones included, and
// "doc/#" every doc's comments. Who belonged where was only ever checked when
// the backend chose which topics to hand a client.
package business

import (
	"context"
	"strings"
	"time"

	boardBusiness "github.com/akashc777/OneCamp/business/Board"
	tableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Request is what the broker asks: which client (by the username its
// sign-in token was checked for), which topic, and whether to publish or
// subscribe.
type Request struct {
	Username string `json:"username"`
	Topic    string `json:"topic"`
	Action   string `json:"action"`
}

// lookupTimeout keeps an answer inside the broker's wait for one (5 s in the
// compose files): past that the broker refuses on its own, and nothing here
// learns why.
const lookupTimeout = 4 * time.Second

// Allowed answers the broker.
func Allowed(ctx context.Context, req Request) bool {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	return live.allowed(ctx, req)
}

// sources is where the answers come from: the database, everywhere but tests.
type sources struct {
	activeUser  func(ctx context.Context, id uuid.UUID) (*userModels.User, error)
	memberTopic func(ctx context.Context, userUUID, topic string) (bool, error)
	readsDoc    func(ctx context.Context, userUUID, docID string) (bool, error)
	readsBoard  func(ctx context.Context, userUUID, boardID string) (bool, error)
	viewsTable  func(ctx context.Context, user *userModels.User, tableID uuid.UUID) (bool, error)
}

var live = sources{
	activeUser:  userDomain.GetActiveUserWithAdminFlagByUserUUID,
	memberTopic: mqttBusiness.IsMemberTopic,
	readsDoc: func(ctx context.Context, userUUID, docID string) (bool, error) {
		user, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, userUUID)
		if err != nil || user == nil {
			return false, err
		}
		doc, err := docBusiness.GetBasicDgraphDocByUUID(ctx, docID, user.Uid)
		if err != nil {
			return false, err
		}
		return doc != nil && doc.Uuid == docID && docBusiness.CanRead(doc, userUUID), nil
	},
	readsBoard: func(ctx context.Context, userUUID, boardID string) (bool, error) {
		user, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, userUUID)
		if err != nil || user == nil {
			return false, err
		}
		board, err := boardBusiness.GetBasicBoardByUUID(ctx, boardID, user.Uid)
		if err != nil {
			return false, err
		}
		return board != nil && board.Uuid == boardID && boardBusiness.CanRead(board, userUUID), nil
	},
	viewsTable: func(ctx context.Context, user *userModels.User, tableID uuid.UUID) (bool, error) {
		return tableBusiness.ViewableBy(ctx, tableBusiness.Actor{UserID: user.Id, IsAdmin: user.IsAdmin}, tableID)
	},
}

func (s sources) allowed(ctx context.Context, req Request) bool {
	// People never publish: what they send goes through the API, which checks
	// it and publishes as the backend.
	if req.Action != "subscribe" {
		return false
	}
	// The broker checked the client's token for this username, which the
	// backend writes as "user_" and the person's id.
	userID, ok := helpers.ParseMqttUsername(req.Username)
	if !ok {
		return false
	}
	// A wildcard covers topics this can't see.
	if strings.ContainsAny(req.Topic, "+#") {
		helpers.LogWarnWithContext(ctx, "business/MqttAccess refused a wildcard subscription to %q by %s", clip(req.Topic), clip(req.Username))
		return false
	}
	user, err := s.activeUser(ctx, userID)
	// A member's client: an external person or a bot never signs in, so a
	// broker token naming one (minted while it wrongly could) reads nothing.
	if err != nil || user == nil || user.Id != userID || !user.IsMember() {
		return false
	}
	userUUID := userID.String()

	if req.Topic == helpers.GetPublicUsersStatusTopic() {
		return true
	}
	kind, id, ok := helpers.ParseMqttTopic(req.Topic)
	if !ok {
		helpers.LogWarnWithContext(ctx, "business/MqttAccess refused a subscription to %q by %s: not a topic this server writes", clip(req.Topic), clip(req.Username))
		return false
	}
	switch kind {
	case helpers.MqttKindActivity:
		return id == userUUID
	case helpers.MqttKindAdmin:
		return user.IsAdmin && req.Topic == helpers.GetMqttTopicForAdminBroadcast()
	case helpers.MqttKindMessage, helpers.MqttKindTyping:
		return answer(ctx, req, func() (bool, error) { return s.memberTopic(ctx, userUUID, req.Topic) })
	}

	// Docs, boards and tables are named by their uuid.
	thing, err := uuid.Parse(id)
	if err != nil || thing.String() != id {
		return false
	}
	switch kind {
	case helpers.MqttKindDoc:
		return answer(ctx, req, func() (bool, error) { return s.readsDoc(ctx, userUUID, id) })
	case helpers.MqttKindBoard:
		return answer(ctx, req, func() (bool, error) { return s.readsBoard(ctx, userUUID, id) })
	case helpers.MqttKindTable:
		return answer(ctx, req, func() (bool, error) { return s.viewsTable(ctx, user, thing) })
	}
	return false
}

// clip shortens what a client chose before it goes in a log line: a topic is
// up to the request's size, and one per subscription is a lot of log.
func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// answer is a lookup's yes, or no when the lookup failed (and why, logged).
func answer(ctx context.Context, req Request, lookup func() (bool, error)) bool {
	yes, err := lookup()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/MqttAccess could not decide a subscription to %q by %s err: %+v", req.Topic, req.Username, err)
		return false
	}
	return yes
}
