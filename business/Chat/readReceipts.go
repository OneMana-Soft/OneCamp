package business

// Read receipts in DMs and group chats: who has seen a conversation, up to
// when. A conversation is seen up to the moment someone last had it open
// (MarkChatSeen); a message is seen by whoever's mark is after it.
//
// Three things decide whether they show, as in Teams: the workspace allows
// them (settings ReadReceiptsEnabled, on unless an admin turns them off), the
// person shares their own (their read_receipts preference: off, they don't
// see anyone else's either), and the conversation is small enough that a
// receipt means something (MaxReceiptPeople). Agents and other bots never
// read, so they're never listed.

import (
	"context"
	"errors"
	"time"

	lastseenBusiness "github.com/akashc777/OneCamp/business/LastSeenChat"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	lastseenDomain "github.com/akashc777/OneCamp/domain/LastSeenChat"
	prefDomain "github.com/akashc777/OneCamp/domain/UserNotificationPreference"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// MaxReceiptPeople is the most people a conversation can have and still show
// read receipts (Teams' limit): past it, a list of who has read what is noise.
const MaxReceiptPeople = 20

// ErrNotInChat is a conversation the person isn't in, or that isn't there.
var ErrNotInChat = errors.New("not in this conversation")

// SeenBy is one person's read receipt: they've seen the conversation up to At.
type SeenBy struct {
	UserUUID string    `json:"user_uuid"`
	At       time.Time `json:"seen_at"`
}

// Receipts is what a person sees of a conversation's read receipts. On is
// false when they don't show at all (the workspace, the person or the size);
// Seen is then empty.
type Receipts struct {
	On   bool     `json:"on"`
	Seen []SeenBy `json:"seen"`
}

// receiptPerson is one of a conversation's people, as receipts need them.
type receiptPerson struct {
	uuid   string
	bot    bool
	shares bool
}

// receiptsFor is the rule, given the facts: what viewer sees of the others'
// receipts. A mark before 1971 is a row made for someone who has never opened
// the conversation, which isn't seeing it.
func receiptsFor(viewer string, people []receiptPerson, seen map[string]time.Time, allowed bool) Receipts {
	out := Receipts{Seen: []SeenBy{}}
	humans := 0
	viewerShares := false
	for _, p := range people {
		if p.bot {
			continue
		}
		humans++
		if p.uuid == viewer {
			viewerShares = p.shares
		}
	}
	if !allowed || !viewerShares || humans > MaxReceiptPeople {
		return out
	}
	out.On = true
	for _, p := range people {
		if p.bot || p.uuid == viewer || !p.shares {
			continue
		}
		if at, ok := seen[p.uuid]; ok && at.Year() > 1970 {
			out.Seen = append(out.Seen, SeenBy{UserUUID: p.uuid, At: at})
		}
	}
	return out
}

// receiptPeople reads a conversation's people and whether each shares read
// receipts, or ErrNotInChat when the viewer isn't one of them.
func receiptPeople(ctx context.Context, viewerDgraphUID, groupingID string) ([]receiptPerson, error) {
	dm, err := GetDgraphDmBasicInfoFromDgraph(ctx, viewerDgraphUID, groupingID)
	if err != nil {
		return nil, err
	}
	if dm == nil || dm.ParticipantIsMember == 0 {
		return nil, ErrNotInChat
	}
	ids := make([]uuid.UUID, 0, len(dm.Participants))
	for _, p := range dm.Participants {
		if p == nil || p.IsBot {
			continue
		}
		if id, err := uuid.Parse(p.Uuid); err == nil {
			ids = append(ids, id)
		}
	}
	prefs, err := prefDomain.LoadByUserIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	people := make([]receiptPerson, 0, len(dm.Participants))
	for _, p := range dm.Participants {
		if p == nil {
			continue
		}
		shares := true // no row yet: the default
		if id, err := uuid.Parse(p.Uuid); err == nil {
			if pref := prefs[id]; pref != nil {
				shares = pref.ReadReceipts
			}
		}
		people = append(people, receiptPerson{uuid: p.Uuid, bot: p.IsBot, shares: shares})
	}
	return people, nil
}

// ChatReceipts is what viewer sees of who has read the conversation.
func ChatReceipts(ctx context.Context, viewer *userModels.UserInfo, groupingID string) (*Receipts, error) {
	people, err := receiptPeople(ctx, viewer.UserDgraphInfo.Uid, groupingID)
	if err != nil {
		return nil, err
	}
	allowed := settingsBusiness.ReadReceiptsEnabled()
	seen := map[string]time.Time{}
	if allowed {
		if seen, err = lastseenDomain.GetLastSeenForGrouping(ctx, groupingID); err != nil {
			return nil, err
		}
	}
	r := receiptsFor(viewer.UserDgraphInfo.Uuid, people, seen, allowed)
	return &r, nil
}

// MarkChatSeen records that viewer has the conversation open now, and tells
// each other person in it who'd see the receipt, live. The mark never moves
// back (domain/LastSeenChat).
func MarkChatSeen(ctx context.Context, viewer *userModels.UserInfo, groupingID string) error {
	people, err := receiptPeople(ctx, viewer.UserDgraphInfo.Uid, groupingID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := lastseenBusiness.CreateOrUpdateLastSeenChat(ctx, groupingID, viewer.UserPostgresInfo.Id, now); err != nil {
		return err
	}
	if !settingsBusiness.ReadReceiptsEnabled() {
		return nil
	}
	var topics []string
	for _, p := range people {
		if p.bot || p.uuid == viewer.UserDgraphInfo.Uuid || !p.shares {
			continue
		}
		// Each of them sees viewer's receipt only if viewer shares it, and
		// the conversation isn't too big: the rule, from their side.
		if r := receiptsFor(p.uuid, people, map[string]time.Time{viewer.UserDgraphInfo.Uuid: now}, true); r.On && len(r.Seen) == 1 {
			topics = append(topics, helpers.GetMqttTopicForUserActivity(p.uuid))
		}
	}
	mqttBusiness.PublishToTopics(topics, mqttStruct.MESSAGE_CHAT_SEEN, mqttStruct.MqttChatSeen{
		ChatGrpId: groupingID,
		UserUuid:  viewer.UserDgraphInfo.Uuid,
		SeenAt:    now,
	})
	return nil
}
