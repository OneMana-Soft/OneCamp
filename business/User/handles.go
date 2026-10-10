package business

// A new member's display name and handle.
//
// Everyone who joins gets a display name, the name on their messages, and a
// handle (users.username), short and unique, derived from that name. Two
// people called Sam are Sam and Sam, @sam and @sam-2. A name collision used to
// rename the second Sam to "sam_1a2b3" everywhere, in the name everyone sees,
// and a name the profile editor then refused to save.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// handleRaces bounds how often a free handle is looked for again after
// another join took the one picked between the read and the write.
const handleRaces = 5

// memberDisplayName is the display name someone joining with name and email
// is given: name cleaned to the person rule, else the address's part before
// the @, else "Member". Pure.
func memberDisplayName(name, email string) string {
	if n := helpers.CleanPersonName(name); n != "" {
		return n
	}
	if n := helpers.CleanPersonName(email); n != "" {
		return n
	}
	return "Member"
}

// firstFreeHandle is the first of base, base-2, base-3, ... that is not in
// taken, compared without case, and not reserved (helpers.HandleIsReserved):
// someone called Admin is @admin-2. Pure.
func firstFreeHandle(base string, taken []string) string {
	used := make(map[string]bool, len(taken))
	for _, h := range taken {
		used[strings.ToLower(h)] = true
	}
	for n := 1; ; n++ {
		if h := helpers.HandleCandidate(base, n); !used[strings.ToLower(h)] && !helpers.HandleIsReserved(h) {
			return h
		}
	}
}

// fallbackHandle is the handle settled for after too many lost races: base
// cut so that a hyphen and suffix still fit within helpers.HandleMaxRunes. It
// used to append them to the whole base, so a long one came out longer than
// any handle may be, and couldn't be saved again. Pure.
func fallbackHandle(base, suffix string) string {
	runes := []rune(helpers.HandleCandidate(base, 1))
	if keep := helpers.HandleMaxRunes - 1 - utf8.RuneCountInString(suffix); len(runes) > keep {
		runes = runes[:keep]
	}
	return strings.TrimRight(string(runes), "._-") + "-" + suffix
}

// claimFreeHandle finds a handle based on base that nobody has and claims it
// with claim, which reports a unique violation on username when another join
// took it first; it then looks again. After handleRaces lost races it settles
// for a random suffix rather than refuse someone joining.
func claimFreeHandle(ctx context.Context, base string, claim func(handle string) error) (string, error) {
	for race := 0; race < handleRaces; race++ {
		taken, err := domain.TakenHandles(ctx, base)
		if err != nil {
			return "", err
		}
		handle := firstFreeHandle(base, taken)
		err = claim(handle)
		if err == nil {
			return handle, nil
		}
		if !domain.IsUniqueViolationOnUsername(err) {
			return "", err
		}
		helpers.LogInfoWithContext(ctx, "business/claimFreeHandle %q was taken by another join; looking again", handle)
	}
	handle := fallbackHandle(base, strings.ReplaceAll(uuid.NewString(), "-", "")[:5])
	return handle, claim(handle)
}

// ensureHandle returns the handle of someone adopted from an external row,
// giving them one derived from their name when the row had none (a GitHub
// sync leaves none). Best effort: a failure is logged and answered with "".
func ensureHandle(ctx context.Context, userID uuid.UUID, name, email string) string {
	if current, err := domain.GetHandle(ctx, userID); err == nil && current != "" {
		return current
	}
	base := helpers.HandleFromName(memberDisplayName(name, email), email)
	handle, err := claimFreeHandle(ctx, base, func(h string) error {
		set, err := domain.SetHandleIfMissing(ctx, userID, h)
		if err == nil && !set {
			return errHandleAlreadySet
		}
		return err
	})
	if errors.Is(err, errHandleAlreadySet) {
		current, _ := domain.GetHandle(ctx, userID)
		return current
	}
	if err != nil {
		helpers.LogWarnWithContext(ctx, "business/ensureHandle %s has no handle: %+v", userID, err)
		return ""
	}
	return handle
}

var errHandleAlreadySet = errors.New("the account has a handle already")

// HandleOnRead is the handle to show for someone whose profile is read,
// giving a member without one theirs on the spot (ensureHandle, from the name
// people see): the startup backfill (BackfillHandles) may not have reached
// them yet, and nobody is shown an empty @handle. An external row or a bot is
// answered with what it has, and so is a read without the member's graph
// record (graph nil or without a uuid): /basicSelfProfile reads Postgres
// alone, and a handle made there would come from the address rather than the
// name, and be kept. user must come from a read that selected is_external
// and is_bot. "" when it can't be had.
func HandleOnRead(ctx context.Context, user *userModels.User, graph *dgraphStruct.DgraphUser) string {
	if user == nil {
		return ""
	}
	if !user.IsMember() || graph == nil || graph.Uuid == "" {
		handle, _ := domain.GetHandle(ctx, user.Id)
		return handle
	}
	return ensureHandle(ctx, user.Id, graph.DisplayName(), user.EmailID)
}

// HandleRefusal is why a handle cannot be had, in words for the person
// choosing it. Taken reports that someone else has it.
type HandleRefusal struct {
	Msg   string
	Taken bool
}

func (e *HandleRefusal) Error() string { return e.Msg }

// ChangeHandle gives someone the handle they chose and returns it as kept.
// Only a handle that changes is checked: against the handle rule, and against
// everyone else's handles, without case. One unchanged, whatever made it, is
// accepted as it is.
func ChangeHandle(ctx context.Context, userID uuid.UUID, chosen string) (string, error) {
	handle := helpers.NormalizeHandle(chosen)
	current, err := domain.GetHandle(ctx, userID)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(handle, current) {
		return current, nil
	}
	if helpers.HandleIsReserved(handle) {
		return "", &HandleRefusal{Msg: fmt.Sprintf("@%s means a group of people in a mention, so it can't be anyone's handle. Try another.", handle)}
	}
	if !helpers.IsValidHandle(handle) {
		return "", &HandleRefusal{Msg: helpers.HandleRuleMessage}
	}
	taken := &HandleRefusal{Msg: fmt.Sprintf("@%s is taken. Try another.", handle), Taken: true}
	if other, err := domain.HandleTakenByAnother(ctx, handle, userID); err != nil {
		return "", err
	} else if other {
		return "", taken
	}
	if err := domain.SetHandle(ctx, userID, handle); err != nil {
		if domain.IsUniqueViolationOnUsername(err) {
			return "", taken
		}
		return "", err
	}
	return handle, nil
}

// RenameMember sets the display name someone chose, as they typed it at sign
// up: on an account adopted from an import, which otherwise keeps the name the
// import gave it. The name must follow the person rule.
func RenameMember(ctx context.Context, userID uuid.UUID, name string) error {
	name = helpers.NormalizePersonName(name)
	if !helpers.IsValidPersonName(name) {
		return fmt.Errorf("%s", helpers.PersonNameRuleMessage)
	}
	current, err := domain.GetDgraphUserInfoByUUID(ctx, userID.String())
	if err != nil || current == nil {
		return fmt.Errorf("no graph node for %s: %w", userID, err)
	}
	// The photo is kept as it is: an update without one would wipe it from
	// every search result the rename propagates to.
	_, err = CreateOrUpdateDgraphUser(ctx, &dgraphStruct.DgraphUser{
		Uuid:         userID.String(),
		UserName:     name,
		UserFullName: name,
		ProfileKey:   current.ProfileKey,
	})
	return err
}

// NameForInvitation is the name to suggest to the person signing up at this
// address: the one an import already knows them by, cleaned to the person
// rule, or "" when nobody does.
func NameForInvitation(ctx context.Context, email string) string {
	name, err := domain.ExternalName(ctx, email)
	if err != nil {
		return ""
	}
	return helpers.CleanPersonName(name)
}
