package business

// A channel post written durably (chat A2): its rows and its events in one
// transaction, then its graph node, then its side effects from the outbox
// (postSinks.go), so a crash between any two steps loses nothing and a retried
// send makes one post.
//
//  1. One Postgres transaction: the posts row, the conversations ledger's
//     messages row (with the id the app gave the post: once per author and
//     channel), and the post's two outbox events, held (postEvents).
//  2. The graph node, as today's CreatePost writes it (writePostNode). If that
//     fails, one transaction takes the rows back and cancels the events.
//  3. The events are released and delivered at once (business/Outbox
//     Deliver): the sinks run on this process now, and the drain on any worker
//     later if they couldn't.
//
// A writer that dies between 1 and 3 leaves the events held; the outbox asks
// PostWritten (the message topic's resolver) whether the node was written, and
// releases them, or takes the rows back and cancels them.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Post"
	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	domain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A post's change is two events, so a slow side effect of one message never
// holds back the next one's live delivery:
//
//   - live, for the realtime sink, in the conversation's order (its key is the
//     conversation's, keyOf on the web: "channel:<uuid>"), so open apps get a
//     channel's posts, edits and deletions as they were made;
//   - the rest (search, the AI edition's embedding, notices, the workspace
//     event), in the post's own order ("post:<uuid>"): its creation, edits
//     and deletion reach search in turn, and two posts' notices go side by
//     side.
//
// The embedding's sink is the AI edition's (postEmbedSink.go): without it, it
// isn't registered, and the outbox passes over a name with no sink.
var (
	liveSinks   = []string{sinkRealtime}
	effectSinks = []string{sinkSearch, sinkEmbed, sinkNotify, sinkEvents}
)

// postEvents is a post's change as its two outbox events. A post an import
// writes (helpers.IsBulkImport) is only indexed, as today's CreatePost does:
// nobody is told of history, and no app is sent it live. Pure.
func postEvents(kind string, post, channel uuid.UUID, actor string, imported bool, postKind string) []outboxBusiness.Entry {
	entities := map[string]string{"message": post.String(), "conversation": channel.String(), "kind": "channel"}
	rest := outboxBusiness.Entry{Topic: MessageTopic, Kind: kind, Key: "post:" + post.String(), Entities: entities, Actor: actor, Sinks: effectSinks}
	if imported {
		rest.Sinks = []string{sinkSearch}
		return []outboxBusiness.Entry{rest}
	}
	// A call's post (calls CALLS-3b) tells nobody, and isn't embedded.
	if postKind == helpers.KindHuddle {
		rest.Sinks = []string{sinkSearch, sinkEvents}
	}
	live := outboxBusiness.Entry{Topic: MessageTopic, Kind: kind, Key: "channel:" + channel.String(), Entities: entities, Actor: actor, Sinks: liveSinks}
	return []outboxBusiness.Entry{live, rest}
}

// The seams the durable write's steps go through, for tests.
var (
	writeRows = writePostRowsOnce
	writeNode = writePostNode
	release   = outboxBusiness.Release
	deliver   = outboxBusiness.Deliver
	takeBack  = takeBackPost
	answered  = sentBefore
)

func beginTx(ctx context.Context) (*sql.Tx, error) {
	return postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
}

// CreatePostDurably writes a channel post as the durable pipeline does
// (business/MessagePipeline, the switch's "channel"). clientMsgID is the id
// the app gave it: a second send with it answers the first post.
func CreatePostDurably(ctx context.Context, postInfo *adapter.InputCreateOrUpdatePostInfo, userInfo *userModels.UserInfo, mentions []*dgraphStruct.DgraphUser, channel *dgraphStruct.DgraphChannel, clientMsgID string) (*adapter.OutputCreatePostForPost, error) {
	postUUID := uuid.New()
	at := helpers.CreateTimeOrNow(ctx)
	author := userInfo.UserPostgresInfo.Id
	if postInfo.Kind != "" {
		ctx = helpers.WithMessageKind(ctx, postInfo.Kind)
	}

	// 1. The rows and the event.
	events, err := writeRows(ctx, postUUID, author, postInfo.ChannelUUID, clientMsgID, at, userInfo.UserDgraphInfo.Uuid)
	if errors.Is(err, errSentAlready) {
		return firstSend(ctx, author, postInfo.ChannelUUID, clientMsgID)
	}
	if err != nil {
		return nil, err
	}

	// 2. The node.
	if _, err := writeNode(ctx, postUUID, postInfo, userInfo, mentions, channel, at); err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreatePostDurably the post's node wasn't written, so its rows are taken back: %+v", err)
		_ = helpers.CompensateOnFailure(ctx, "the rows and event of post "+postUUID.String(), func(c context.Context) error {
			return takeBack(c, postUUID, events)
		})
		return nil, err
	}

	// 3. Its side effects.
	for _, id := range events {
		if err := release(ctx, id); err != nil {
			// Held, it is the resolver's: the node is written, so it releases it.
			helpers.LogErrorWithContext(ctx, "business/CreatePostDurably event %d of post %s wasn't released, so the drain will: %+v", id, postUUID, err)
			continue
		}
		deliver(id)
	}
	return &adapter.OutputCreatePostForPost{Uuid: postUUID.String(), PostCreated: at}, nil
}

// errSentAlready is a send whose client id has a message already.
var errSentAlready = errors.New("business/Post: this send was made already")

// ErrStillBeingSent is a send tried again while its first try's post has no
// node yet: that try is still going, or its writer died and the resolver will
// take its rows back. business/Send answers it as Once does a send in flight.
var ErrStillBeingSent = errors.New("business/Post: the first try of this send hasn't finished")

// firstSend answers a send tried again with its client id: the first try's
// post, once its node is written. Until then the answer is ErrStillBeingSent
// and the app tries again, so a retry is never told of a post that may never
// be there.
func firstSend(ctx context.Context, author, channel uuid.UUID, clientMsgID string) (*adapter.OutputCreatePostForPost, error) {
	first, err := answered(ctx, author, channel, clientMsgID)
	if err != nil {
		return nil, err
	}
	if _, err := readPost(ctx, first.Uuid); err != nil {
		if errors.Is(err, domain.ErrPostGone) {
			return nil, ErrStillBeingSent
		}
		return nil, err
	}
	return first, nil
}

// writePostRowsOnce is step 1 in a transaction of its own: the events' ids,
// or errSentAlready when the client id has a message already.
func writePostRowsOnce(ctx context.Context, postUUID, author, channel uuid.UUID, clientMsgID string, at time.Time, authorUUID string) ([]int64, error) {
	tx, err := beginTx(ctx)
	if err != nil {
		return nil, err
	}
	events, err := writePostRows(ctx, tx, postUUID, author, channel, clientMsgID, at, authorUUID)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return events, tx.Commit()
}

// writePostRows is step 1, inside tx: the posts row, the ledger row and the
// events. errSentAlready when the ledger has a message with the client id.
func writePostRows(ctx context.Context, tx *sql.Tx, postUUID, author, channel uuid.UUID, clientMsgID string, at time.Time, authorUUID string) ([]int64, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if _, err := tx.ExecContext(cctx, `INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, postUUID, channel, author); err != nil {
		return nil, err
	}
	// A channel's conversation row is the channel's own id (A9), made here if
	// the index hasn't been filled in for it yet.
	if _, err := tx.ExecContext(cctx, `
		INSERT INTO conversations (id, kind, ref, created_at) VALUES ($1, 'channel', $2, NOW())
		ON CONFLICT DO NOTHING`, channel, channel.String()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(cctx, `
		INSERT INTO messages (id, conversation_id, author_id, client_msg_id, created_at)
		SELECT $1, c.id, $3, $4, $5 FROM conversations c WHERE c.ref = $2`, postUUID, channel.String(), author, ledgerClientID(clientMsgID), at); err != nil {
		if helpers.IsUniqueViolation(err) {
			return nil, errSentAlready
		}
		return nil, err
	}
	var events []int64
	for _, e := range postEvents(MessageCreated, postUUID, channel, authorUUID, helpers.IsBulkImport(ctx), helpers.MessageKind(ctx)) {
		id, err := outboxBusiness.Add(cctx, tx, e)
		if err != nil {
			return nil, err
		}
		events = append(events, id)
	}
	return events, nil
}

// clientIDFormat is the client ids a send is made once by: up to 64 letters,
// digits, - and _ (business/Send's Once takes the same).
var clientIDFormat = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ledgerClientID is the client id the ledger keeps for a send: the app's, or
// none when it isn't one Once takes, so an id too long for the ledger never
// refuses the post (Once ignores it, as it always has). Pure.
func ledgerClientID(id string) any {
	if !clientIDFormat.MatchString(id) {
		return nil
	}
	return id
}

// sentBefore is the post a send with this client id made already, as the
// first send answered it.
func sentBefore(ctx context.Context, author, channel uuid.UUID, clientMsgID string) (*adapter.OutputCreatePostForPost, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var id uuid.UUID
	var at time.Time
	err := postgresInit.DBConn.SqlDB.QueryRowContext(cctx, `
		SELECT m.id, m.created_at FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.author_id = $1 AND c.ref = $2 AND m.client_msg_id = $3`, author, channel.String(), clientMsgID).Scan(&id, &at)
	if err != nil {
		return nil, fmt.Errorf("business/Post: the send made already couldn't be read: %w", err)
	}
	return &adapter.OutputCreatePostForPost{Uuid: id.String(), PostCreated: at}, nil
}

// takeBackPost undoes step 1 for a post whose node wasn't written: its rows go
// and its events are cancelled, in one transaction.
func takeBackPost(ctx context.Context, postUUID uuid.UUID, events []int64) error {
	tx, err := beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if _, err := tx.ExecContext(cctx, `DELETE FROM messages WHERE id = $1`, postUUID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(cctx, `DELETE FROM posts WHERE id = $1`, postUUID); err != nil {
		return err
	}
	for _, id := range events {
		if err := outboxBusiness.Cancel(cctx, tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PostWritten is the message topic's answer for a channel post's held event
// (business/MessagePipeline asks it): true when the post's node is there; when
// it isn't, its writer died before writing it, so its rows are taken back and
// the answer is false.
func PostWritten(ctx context.Context, postID string) (bool, error) {
	_, err := readPost(ctx, postID)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, domain.ErrPostGone) {
		return false, err
	}
	id, perr := uuid.Parse(postID)
	if perr != nil {
		return false, nil
	}
	// Its events are the resolver's to cancel.
	return false, takeBackPost(ctx, id, nil)
}
