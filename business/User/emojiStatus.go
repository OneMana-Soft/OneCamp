package business

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	adapterUser "github.com/akashc777/OneCamp/adapter/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A person's status is an emoji, a few words and when it clears. This is the
// one rule for what one may be; the web's status menu keeps the same limits
// (lib/status/statusRule.ts) and so never sends what this refuses.

// StatusTextMax is how long a status's words may be, in characters as the web
// counts them too (code points, so an emoji in the words counts once).
const StatusTextMax = 100

// statusEmojiID is an emoji picker's id ("sandwich", "+1", "flag-in"): the
// picker's longest is 54 characters.
var statusEmojiID = regexp.MustCompile(`^[a-z0-9_+-]{1,64}$`)

// statusClears are the "Clear after" choices a status may be set with.
var statusClears = []string{
	helpers.UserEmojiStatusExpiryIn30M,
	helpers.UserEmojiStatusExpiryIn1H,
	helpers.UserEmojiStatusExpiryIn4H,
	helpers.UserEmojiStatusExpiryInToday,
	helpers.UserEmojiStatusExpiryInWeek,
	helpers.UserEmojiStatusExpiryInCustom,
}

// StatusRefused is a status the rule refuses, in words the person can act on;
// it is answered as a 400 with them.
type StatusRefused struct{ msg string }

func (e *StatusRefused) Error() string { return e.msg }

func refuseStatus(msg string) error { return &StatusRefused{msg: msg} }

// CheckEmojiStatus is the status asked for, as it is kept, or a
// *StatusRefused: an emoji from the picker, 1 to StatusTextMax characters of
// words, and when it clears, either a chosen time still to come (epoch
// seconds) or a "Clear after" choice counted from now where the person is.
func CheckEmojiStatus(in *adapterUser.UserEmojiStatusInput, now time.Time) (*dgraphStruct.DgraphUserStatusEmoji, error) {
	if in == nil {
		return nil, refuseStatus("Write a few words for your status.")
	}
	emoji := strings.TrimSpace(in.EmojiUuid)
	if !statusEmojiID.MatchString(emoji) {
		return nil, refuseStatus("Choose an emoji for your status.")
	}
	text := strings.TrimSpace(in.StatusDesc)
	if text == "" {
		return nil, refuseStatus("Write a few words for your status.")
	}
	if utf8.RuneCountInString(text) > StatusTextMax {
		return nil, refuseStatus(fmt.Sprintf("Keep your status to %d characters.", StatusTextMax))
	}
	if !slices.Contains(statusClears, in.ExpiryTimeIn) {
		return nil, refuseStatus("Choose when your status clears.")
	}

	var ends time.Time
	switch {
	case in.ExpiryTimeAt != "":
		secs, err := strconv.ParseInt(in.ExpiryTimeAt, 10, 64)
		if err != nil {
			return nil, refuseStatus("Choose when your status clears.")
		}
		ends = time.Unix(secs, 0)
		if !ends.After(now) {
			return nil, refuseStatus("Choose a time that hasn't passed.")
		}
	case in.ExpiryTimeIn == helpers.UserEmojiStatusExpiryInCustom:
		return nil, refuseStatus("Choose when your status clears.")
	default:
		loc, err := time.LoadLocation(in.TimeZone)
		if err != nil {
			return nil, refuseStatus("Your time zone isn't one the server knows, so your status can't clear on time.")
		}
		ends = helpers.GetUserEmojiExipryTime(in.ExpiryTimeIn, now.In(loc))
	}

	return &dgraphStruct.DgraphUserStatusEmoji{
		EmojiUuid: emoji,
		EmojiDesc: text,
		ExpiryAt:  &ends,
		ExpiryIn:  in.ExpiryTimeIn,
	}, nil
}
