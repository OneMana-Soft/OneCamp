package Calendar

import (
	"net/http"

	business "github.com/akashc777/OneCamp/business/Calendar"
	"github.com/akashc777/OneCamp/helpers"
)

// YourWeek is where the asker's own time went in a stretch (calendar B3,
// gcal-new-time-insights): meetings one to one and in groups, focus, free
// working time, invitations to answer, the usual week beside it and the people
// they meet most. Only ever the asker's own: it takes nobody's id.
// GET /calendar/insights?from=…&to=… (RFC 3339, at most 31 days apart)
func YourWeek(w http.ResponseWriter, r *http.Request) {
	u, ok := member(w, r)
	if !ok {
		return
	}
	from, okFrom := queryTime(r, "from")
	to, okTo := queryTime(r, "to")
	if !okFrom || !okTo || from == nil || to == nil || !to.After(*from) || to.Sub(*from) > business.MaxWeekSpan {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Say from when to when, at most 31 days apart, as times like 2026-10-26T00:00:00Z."})
		return
	}
	week, err := business.YourWeek(r.Context(), u, *from, *to)
	if err != nil {
		writeCalendarError(w, r, "YourWeek", err)
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": week})
}
