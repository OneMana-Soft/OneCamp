package helpers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// SeatLimit is how many people this workspace's licence covers, stamped by
// the build server for a free licence (-ldflags "-X .../helpers.SeatLimit=25").
// Empty or 0 means unlimited: every paid licence, and any build from source.
var SeatLimit = ""

// MemberSeatLimit is SeatLimit as a number, 0 for unlimited. Pure.
func MemberSeatLimit() int {
	return parseSeatLimit(SeatLimit)
}

func parseSeatLimit(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// SeatLimitError is returned when a new or returning member would take the
// workspace past its licence. Its message is written for the person who sees
// it: it says what happened and what an admin can do.
type SeatLimitError struct{ Limit int }

func (e *SeatLimitError) Error() string {
	return fmt.Sprintf("This workspace is on OneCamp's free plan, which has room for %d people, and it is full. "+
		"An admin can deactivate someone who has left, or remove the limit with a lifetime licence at onemana.dev/pricing.", e.Limit)
}

// IsSeatLimit reports whether err is (or wraps) a SeatLimitError.
func IsSeatLimit(err error) bool {
	var e *SeatLimitError
	return errors.As(err, &e)
}

// WriteSeatLimit answers a request that ran into the seat limit with a 403
// carrying the limit's own message, and reports whether it did. Every place a
// person can be added calls it first, so a full workspace always says why.
func WriteSeatLimit(w http.ResponseWriter, err error) bool {
	var e *SeatLimitError
	if !errors.As(err, &e) {
		return false
	}
	WriteJSON(w, http.StatusForbidden, Envolope{
		"msg":    e.Error(),
		"status": "failed",
		"code":   "seat_limit",
	})
	return true
}
