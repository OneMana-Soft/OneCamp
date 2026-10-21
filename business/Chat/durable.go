package business

// A DM or group message written durably (chat A2), as a channel post is
// (business/Post durable.go): its rows and its events in one transaction,
// then its graph node, then its side effects from the outbox (chatSinks.go),
// so a crash between any two steps loses nothing and a retried send makes one
// message.
//
//  1. One Postgres transaction: the chats row, the conversations ledger's
//     messages row (with the id the app gave the message: once per author
//     and conversation), and the message's two outbox events, held.
//  2. The graph node, as today's writers write it (writeDMNode,
//     writeGroupNode). If that fails, one transaction takes the rows back and
//     cancels the events. When the message made its conversation, what a new
//     conversation sets up beside it (startDM, startGroup) follows, as today.
//  3. The events are released, the live one saying whom a new conversation
//     is for, and delivered at once (business/Outbox Deliver).
//
// A writer that dies between 1 and 3 leaves the events held; the outbox asks
// ChatWritten (the message topic's resolver, for DMs and groups) whether the
// node was written, and releases them, or takes the rows back and cancels
// them. Released so, the live event no longer knows the conversation was new,
// and its people's open apps learn of it when they next load.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Chat"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	domain "github.com/akashc777/OneCamp/domain/Chat"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	"github.com/akashc777/OneCamp/models/pgexec"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A message's change is two events, as a post's is, so a slow side effect of
// one message never holds back the next one's live delivery:
//
//   - live, for access and realtime, in the conversation's order ("dm:" or
//     "group:" and its grouping id, keyOf on the web), so a new conversation's
//     people are told it's theirs before its first message reaches them;
//   - the rest (search, the AI edition's embedding, notices, the workspace
//     event), in the message's own order ("chat:<uuid>").
//
// The embedding's sink is the AI edition's (chatEmbedSink.go): without it, it
// isn't registered, and the outbox passes over a name with no sink.
var (
	chatLiveSinks   = []string{sinkChatAccess, sinkChatRealtime}
	chatEffectSinks = []string{sinkChatSearch, sinkChatEmbed, sinkChatNotify, sinkChatEvents}
)

// chatEvents is a message's change as its two outbox events. A message an
// import writes (helpers.IsBulkImport) is only indexed, as today's writers do:
// nobody is told of history, and no app is sent it live. Pure.
func chatEvents(kind, conversation string, chat uuid.UUID, actor string, imported bool) []outboxBusiness.Entry {
	entities := map[string]string{"message": chat.String(), "conversation": conversation, "kind": kind}
	rest := outboxBusiness.Entry{Topic: MessageTopic, Kind: MessageCreated, Key: "chat:" + chat.String(), Entities: entities, Actor: actor, Sinks: chatEffectSinks}
	if imported {
		rest.Sinks = []string{sinkChatSearch}
		return []outboxBusiness.Entry{rest}
	}
	live := outboxBusiness.Entry{Topic: MessageTopic, Kind: MessageCreated, Key: kind + ":" + conversation, Entities: entities, Actor: actor, Sinks: chatLiveSinks}
	return []outboxBusiness.Entry{live, rest}
}

// The seams the durable writes' own steps go through, for tests (the steps
// they share with today's writers are chatBusiness.go's).
var (
	writeChatRows  = writeChatRowsOnce
	releaseEvent   = outboxBusiness.ReleaseWith
	deliverEvent   = outboxBusiness.Deliver
	takeBackChatFn = takeBackChat
	chatAnswered   = chatSentBefore
)

// CreateDMDurably writes a DM as the durable pipeline does (business/
// MessagePipeline, the switch's "dm"). clientMsgID is the id the app gave
// it: a second send with it answers the first message.
func CreateDMDurably(ctx context.Context, chatInfo *adapter.ChatInfo, userInfo *userModels.UserInfo, sendToDgraph *dgraphStruct.DgraphUser, toUUID uuid.UUID, mentions []*dgraphStruct.DgraphUser, clientMsgID string) (*adapter.OutputCreateChatForChat, error) {
	chatUUID := uuid.New()
	at := helpers.CreateTimeOrNow(ctx)
	author := userInfo.UserPostgresInfo.Id
	grp := helpers.GetGroupingId(author.String(), chatInfo.ToUuid)

	// 1. The rows and the events.
	events, err := writeChatRows(ctx, mqttStruct.CONVERSATION_KIND_DM, grp, chatUUID, author, clientMsgID, at, userInfo.UserDgraphInfo.Uuid)
	if errors.Is(err, errChatSentAlready) {
		return firstChatSend(ctx, author, grp, clientMsgID)
	}
	if err != nil {
		return nil, err
	}

	// 2. The node, and a new conversation's setup.
	_, dmUID, err := writeDM(ctx, chatUUID, chatInfo, userInfo, sendToDgraph, grp, mentions, at)
	if err != nil {
		return nil, takeBackUnwritten(ctx, chatUUID, events, err)
	}
	var opened []string
	if dmUID != "" {
		startNewDM(ctx, userInfo, sendToDgraph, toUUID, grp, at)
		opened = []string{userInfo.UserDgraphInfo.Uuid, sendToDgraph.Uuid}
	}
	markRead(ctx, "business/CreateDMDurably", grp, userInfo, at)

	// 3. Its side effects.
	releaseChatEvents(ctx, chatUUID, events, opened)
	return &adapter.OutputCreateChatForChat{Uuid: chatUUID.String(), ChatCreated: at}, nil
}

// CreateGroupMessageDurably writes a group message as the durable pipeline
// does (the switch's "group"). participants are the group's people
// (business/Send PrepareGroupMessage): an existing group's, or a new one's,
// whose id they make. clientMsgID is as CreateDMDurably's.
func CreateGroupMessageDurably(ctx context.Context, chatInfo *adapter.ChatInfo, userInfo *userModels.UserInfo, mentions []*dgraphStruct.DgraphUser, participants []*dgraphStruct.DgraphUser, clientMsgID string) (*adapter.OutputCreateChatForChat, error) {
	chatUUID := uuid.New()
	at := helpers.CreateTimeOrNow(ctx)
	author := userInfo.UserPostgresInfo.Id
	isNewGroup := chatInfo.GrpUuid == ""
	people := uuidsOf(participants)
	if isNewGroup {
		grp, err := helpers.GenerateGroupID(people)
		if err != nil {
			return nil, err
		}
		chatInfo.GrpUuid = grp
	}

	// 1. The rows and the events.
	events, err := writeChatRows(ctx, mqttStruct.CONVERSATION_KIND_GROUP, chatInfo.GrpUuid, chatUUID, author, clientMsgID, at, userInfo.UserDgraphInfo.Uuid)
	if errors.Is(err, errChatSentAlready) {
		return firstChatSend(ctx, author, chatInfo.GrpUuid, clientMsgID)
	}
	if err != nil {
		return nil, err
	}

	// 2. The node, and a new conversation's setup.
	var newMembers []*dgraphStruct.DgraphUser
	if isNewGroup {
		newMembers = participants
	}
	_, dmUID, err := writeGroup(ctx, chatUUID, chatInfo, userInfo, mentions, newMembers, at)
	if err != nil {
		return nil, takeBackUnwritten(ctx, chatUUID, events, err)
	}
	var opened []string
	if dmUID != "" {
		startNewGroup(ctx, userInfo, chatInfo.GrpUuid, people, at)
		opened = people
	}
	markRead(ctx, "business/CreateGroupMessageDurably", chatInfo.GrpUuid, userInfo, at)

	// 3. Its side effects.
	releaseChatEvents(ctx, chatUUID, events, opened)
	return &adapter.OutputCreateChatForChat{Uuid: chatUUID.String(), ChatCreated: at}, nil
}

// takeBackUnwritten takes back the rows and events of a message whose node
// wasn't written, and answers the node's error.
func takeBackUnwritten(ctx context.Context, chatUUID uuid.UUID, events []int64, nodeErr error) error {
	helpers.LogErrorWithContext(ctx, "business/Chat durable: the message's node wasn't written, so its rows are taken back: %+v", nodeErr)
	_ = helpers.CompensateOnFailure(ctx, "the rows and events of message "+chatUUID.String(), func(c context.Context) error {
		return takeBackChatFn(c, chatUUID, events)
	})
	return nodeErr
}

// releaseChatEvents releases a written message's events and delivers each at
// once; the live one (the first) says whom a new conversation is for.
func releaseChatEvents(ctx context.Context, chatUUID uuid.UUID, events []int64, opened []string) {
	for i, id := range events {
		var more map[string]string
		if i == 0 && len(events) > 1 && len(opened) > 0 {
			more = map[string]string{"opened": strings.Join(opened, ",")}
		}
		if err := releaseEvent(ctx, id, more); err != nil {
			// Held, it is the resolver's: the node is written, so it releases it.
			helpers.LogErrorWithContext(ctx, "business/Chat durable: event %d of message %s wasn't released, so the drain will: %+v", id, chatUUID, err)
			continue
		}
		deliverEvent(id)
	}
}

// errChatSentAlready is a send whose client id has a message already.
var errChatSentAlready = errors.New("business/Chat: this send was made already")

// ErrStillBeingSent is a send tried again while its first try's message has
// no node yet: that try is still going, or its writer died and the resolver
// will take its rows back. business/Send answers it as Once does a send in
// flight.
var ErrStillBeingSent = errors.New("business/Chat: the first try of this send hasn't finished")

// firstChatSend answers a send tried again with its client id: the first
// try's message, once its node is written; until then ErrStillBeingSent, and
// the app tries again.
func firstChatSend(ctx context.Context, author uuid.UUID, conversation, clientMsgID string) (*adapter.OutputCreateChatForChat, error) {
	first, err := chatAnswered(ctx, author, conversation, clientMsgID)
	if err != nil {
		return nil, err
	}
	if _, err := readChat(ctx, first.Uuid); err != nil {
		if errors.Is(err, domain.ErrChatGone) {
			return nil, ErrStillBeingSent
		}
		return nil, err
	}
	return first, nil
}

// writeChatRowsOnce is step 1 in a transaction of its own: the events' ids,
// or errChatSentAlready when the client id has a message already.
func writeChatRowsOnce(ctx context.Context, kind, conversation string, chatUUID, author uuid.UUID, clientMsgID string, at time.Time, actor string) ([]int64, error) {
	var events []int64
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		var err error
		events, err = writeChatRowsIn(ctx, kind, conversation, chatUUID, author, clientMsgID, at, actor)
		return err
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// writeChatRowsIn is step 1, inside ctx's transaction (pgexec.InTx): the
// chats row, the ledger row (and the conversation's, made here if the index
// hasn't been filled in for it yet) and the events.
func writeChatRowsIn(ctx context.Context, kind, conversation string, chatUUID, author uuid.UUID, clientMsgID string, at time.Time, actor string) ([]int64, error) {
	if _, err := pgexec.Exec(ctx, `INSERT INTO chats (id, created_by, grp_id) VALUES ($1, $2, $3)`, chatUUID, author, conversation); err != nil {
		return nil, err
	}
	// A DM's or group's conversation row has an id of its own (A9's index
	// makes it the same way: domain/Conversation rowID).
	if _, err := pgexec.Exec(ctx, `
		INSERT INTO conversations (id, kind, ref, created_by, created_at) VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT DO NOTHING`, uuid.New(), kind, conversation, author); err != nil {
		return nil, err
	}
	if _, err := pgexec.Exec(ctx, `
		INSERT INTO messages (id, conversation_id, author_id, client_msg_id, created_at)
		SELECT $1, c.id, $3, $4, $5 FROM conversations c WHERE c.ref = $2`, chatUUID, conversation, author, chatClientID(clientMsgID), at); err != nil {
		if helpers.IsUniqueViolation(err) {
			return nil, errChatSentAlready
		}
		return nil, err
	}
	tx, err := pgexec.Current(ctx)
	if err != nil {
		return nil, err
	}
	var events []int64
	// A message's events are two at most (live, and the rest), as a post's.
	for _, e := range chatEvents(kind, conversation, chatUUID, actor, helpers.IsBulkImport(ctx)) {
		id, err := outboxBusiness.Add(ctx, tx, e)
		if err != nil {
			return nil, err
		}
		events = append(events, id)
	}
	return events, nil
}

// chatClientIDFormat is the client ids a send is made once by: up to 64
// letters, digits, - and _ (business/Send's Once takes the same).
var chatClientIDFormat = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// chatClientID is the client id the ledger keeps for a send: the app's, or
// none when it isn't one Once takes, so an id too long for the ledger never
// refuses the message. Pure.
func chatClientID(id string) any {
	if !chatClientIDFormat.MatchString(id) {
		return nil
	}
	return id
}

// chatSentBefore is the message a send with this client id made already, as
// the first send answered it.
func chatSentBefore(ctx context.Context, author uuid.UUID, conversation, clientMsgID string) (*adapter.OutputCreateChatForChat, error) {
	first, found, err := pgexec.QueryOne(ctx, `
		SELECT m.id, m.created_at FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.author_id = $1 AND c.ref = $2 AND m.client_msg_id = $3`, func(r pgexec.Row) (*adapter.OutputCreateChatForChat, error) {
		var id uuid.UUID
		var at time.Time
		if err := r.Scan(&id, &at); err != nil {
			return nil, err
		}
		return &adapter.OutputCreateChatForChat{Uuid: id.String(), ChatCreated: at}, nil
	}, author, conversation, clientMsgID)
	if err == nil && !found {
		err = errors.New("no message has this send's id")
	}
	if err != nil {
		return nil, fmt.Errorf("business/Chat: the send made already couldn't be read: %w", err)
	}
	return first, nil
}

// takeBackChat undoes step 1 for a message whose node wasn't written: its
// rows go and its events are cancelled, in one transaction.
func takeBackChat(ctx context.Context, chatUUID uuid.UUID, events []int64) error {
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		conversation, _, err := pgexec.QueryOne(ctx, `DELETE FROM messages WHERE id = $1 RETURNING conversation_id`,
			func(r pgexec.Row) (c uuid.NullUUID, err error) {
				err = r.Scan(&c)
				return
			}, chatUUID)
		if err != nil {
			return err
		}
		if _, err := pgexec.Exec(ctx, `DELETE FROM chats WHERE id = $1`, chatUUID); err != nil {
			return err
		}
		// A conversation the message's rows made, which nothing else has since
		// (no message, nobody in it), goes with them: it never had a graph node.
		if conversation.Valid {
			if _, err := pgexec.Exec(ctx, `
				DELETE FROM conversations c WHERE c.id = $1 AND c.kind <> 'channel'
				AND NOT EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = c.id)
				AND NOT EXISTS (SELECT 1 FROM conversation_members cm WHERE cm.conversation_id = c.id)`, conversation.UUID); err != nil {
				return err
			}
		}
		tx, err := pgexec.Current(ctx)
		if err != nil {
			return err
		}
		// A message's events are two at most, as a post's.
		for _, id := range events {
			if err := outboxBusiness.Cancel(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// ChatWritten is the message topic's answer for a DM's or group message's
// held event (business/MessagePipeline asks it): true when the message's node
// is there; when it isn't, its writer died before writing it, so its rows are
// taken back and the answer is false.
func ChatWritten(ctx context.Context, chatID string) (bool, error) {
	_, err := readChat(ctx, chatID)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, domain.ErrChatGone) {
		return false, err
	}
	id, perr := uuid.Parse(chatID)
	if perr != nil {
		return false, nil
	}
	// Its events are the resolver's to cancel.
	return false, takeBackChat(ctx, id, nil)
}
