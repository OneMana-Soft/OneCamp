package business

// The read check of a push that waits while its person is in OneCamp (A4 H4,
// business/Notify): their own marker where the message was written, read as
// Position reads it, for them alone. Given to Notify at init, as Notify can't
// import this package (it reaches Notify through business/Chat).

import (
	"context"
	"errors"
	"time"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/google/uuid"
)

func init() {
	notifyBusiness.ReadWith(readSince)
}

// readSince is when userID last read where (nil when they have no marker
// there), and whether they may read it: someone who left, or who can't read
// the conversation any more, may not.
func readSince(ctx context.Context, userID string, where notifyBusiness.Place) (*time.Time, bool, error) {
	id, err := uuid.Parse(userID)
	if err != nil {
		return nil, false, nil
	}
	who, err := userDomain.ActorByUUID(ctx, id)
	if errors.Is(err, userDomain.ErrNoActor) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	ref := conversationBusiness.ChatRef(where.ID)
	if where.Kind == notifyBusiness.PlaceChannel {
		ref = conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: where.ID}
	}
	p, err := States.Position(ctx, who, ref)
	if errors.Is(err, conversationBusiness.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	return p.Since, true, nil
}
