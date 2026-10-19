package domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/google/uuid"
)

// CreateChatAt is CreateChat for a message written at at, not now: a copy a
// shared history makes keeps its original's time, so it is never counted
// unread, and retention reads its age as the original's.
func CreateChatAt(ctx context.Context, chatID uuid.UUID, createdBy uuid.UUID, grpID string, at time.Time) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, `
		INSERT INTO chats (id, created_by, grp_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $4)`, chatID, createdBy, grpID, at)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CreateChatAt Failed to create the chat row err: %+v", err)
	}
	return err
}

// LinkReplyTo makes the message chatUUID quote the message parentUUID
// (chat_reply_to), when both are messages of one conversation: a shared
// history's copy quoting the copy of what its original quoted. Nothing is
// written otherwise.
func LinkReplyTo(ctx context.Context, chatUUID, parentUUID string) error {
	_, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query: `query q($c: string, $p: string) {
			c as var(func: eq(chat_uuid, $c)) { cd as chat_dm }
			p as var(func: eq(chat_uuid, $p)) @filter(uid_in(chat_dm, uid(cd)))
		}`,
		Vars:      map[string]string{"$c": chatUUID, "$p": parentUUID},
		Mutations: []*api.Mutation{{SetNquads: []byte(`uid(c) <chat_reply_to> uid(p) .`), Cond: "@if(eq(len(c), 1) AND eq(len(p), 1))"}},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/LinkReplyTo err: %+v", err)
	}
	return err
}
