package Calendar

import (
	"net/http"
	"time"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
)

// ListCalendarRepeats is what repeats on the asker's calendar beside their
// events, in a stretch of time (calendar head 9, gcal-1b): the check-ins of
// the channels they are in, at each time they ask, and the dates their open
// tasks that repeat on a schedule will be due next.
// GET /calendar/repeats?from=…&to=… (RFC 3339, at most 62 days apart)
func ListCalendarRepeats(w http.ResponseWriter, r *http.Request) {
	u, ok := member(w, r)
	if !ok {
		return
	}
	from, okFrom := queryTime(r, "from")
	to, okTo := queryTime(r, "to")
	if !okFrom || !okTo || from == nil || to == nil || !to.After(*from) || to.Sub(*from) > business.MaxRepeatsSpan {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say from when to when, at most 62 days apart, as times like 2026-10-16T00:00:00Z."})
		return
	}
	view, err := business.Repeats(r.Context(), u, *from, *to, time.Now())
	if err != nil {
		writeCalendarError(w, r, "ListCalendarRepeats", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": view})
}
