package business

// Members in a call through a link they opened as themselves (calls HU13): at
// a meeting's link or a room's address, or through one call's invite.
// business/Guest owns links and gives the answer; every call check here and
// the Access Resolver's call and thread kinds read that one answer.

import (
	"context"

	"github.com/google/uuid"
)

var roomsAdmittedTo func(ctx context.Context, user uuid.UUID) (map[string]bool, error)

// AnswerAdmissionsWith is how business/Guest gives its answer, once, as it starts.
func AnswerAdmissionsWith(answer func(ctx context.Context, user uuid.UUID) (map[string]bool, error)) {
	roomsAdmittedTo = answer
}

// RoomsAdmittedTo answers the call rooms a member may be in now through links
// they opened as themselves: none until business/Guest answers.
func RoomsAdmittedTo(ctx context.Context, user uuid.UUID) (map[string]bool, error) {
	if roomsAdmittedTo == nil || user == uuid.Nil {
		return map[string]bool{}, nil
	}
	return roomsAdmittedTo(ctx, user)
}
