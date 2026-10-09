package Calendar

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Calendar"
	business "github.com/akashc777/OneCamp/business/Calendar"
	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func CreateEventController(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input adapter.CreateOrUpdateEventInput

	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/CreateEventController Failed to parse the body of the req err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Failed to parse the body of the req",
			"err": err,
		})
		return
	}

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	if !ok {
		helpers.LogErrorWithContext(ctx, "controllers/CreateEventController Failed getting user from context")
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	output, err := business.CreateEvent(ctx, &userInfo, input)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Internal Error", "err": err})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Created event!", "data": output})
}

func UpdateEventController(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var input adapter.CreateOrUpdateEventInput

	eventUUIdString := chi.URLParam(r, "eventId")

	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/UpdateEventController binding err: %+v", err)
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse the body of the req", "err": err})
		return
	}

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	if !ok {
		helpers.LogErrorWithContext(ctx, "controllers/UpdateEventController Failed getting user from context")
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	if len(eventUUIdString) > 5 && eventUUIdString[:5] == "gcal-" {
		googleEventId := eventUUIdString[5:]
		err = integrationBusiness.UpdateGoogleCalendarEvent(ctx, googleEventId, input, &userInfo)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/UpdateEventController Google Sync failed err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to update Google Calendar event", "err": err})
			return
		}
	} else {
		eventUUID, err := uuid.Parse(eventUUIdString)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/UpdateEventController Parse string to UUid failed err: %+v", err)
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid event id format", "err": err})
			return
		}

		err = business.UpdateEvent(ctx, eventUUID, input, &userInfo)
		if err != nil {
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Internal Error", "err": err})
			return
		}
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Updated event!"})
}

func DeleteEventController(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	eventUUIdString := chi.URLParam(r, "eventId")

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	if !ok {
		helpers.LogErrorWithContext(ctx, "controllers/DeleteEventController Failed getting user from context")
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	if len(eventUUIdString) > 5 && eventUUIdString[:5] == "gcal-" {
		googleEventId := eventUUIdString[5:]
		err := integrationBusiness.DeleteGoogleCalendarEvent(ctx, googleEventId, &userInfo)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/DeleteEventController Google Sync failed err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to delete Google Calendar event", "err": err})
			return
		}
	} else {
		eventUUID, err := uuid.Parse(eventUUIdString)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/DeleteEventController Parse string to UUid failed err: %+v", err)
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid event id format", "err": err})
			return
		}

		err = business.DeleteEvent(ctx, eventUUID, &userInfo)
		if errors.Is(err, business.ErrNotEventCreator) {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": err.Error()})
			return
		}
		if err != nil {
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Internal Error", "err": err})
			return
		}
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Deleted event!"})
}

func GetEventsListController(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	if !ok {
		helpers.LogErrorWithContext(ctx, "controllers/GetEventsListController Failed getting user from context")
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	var startDate, endDate *time.Time
	startDateStr := r.URL.Query().Get("startDate")
	endDateStr := r.URL.Query().Get("endDate")

	if startDateStr == "" || endDateStr == "" {
		helpers.LogErrorWithContext(ctx, "controllers/GetEventsListController Missing startDate or endDate parameters")
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "startDate and endDate are required query parameters to prevent unoptimized queries"})
		return
	}

	parsedStart, errStart := time.Parse(time.RFC3339Nano, startDateStr)
	if errStart != nil {
		parsedStart, _ = time.Parse(time.RFC3339, startDateStr)
	}
	parsedEnd, errEnd := time.Parse(time.RFC3339Nano, endDateStr)
	if errEnd != nil {
		parsedEnd, _ = time.Parse(time.RFC3339, endDateStr)
	}

	if parsedStart.IsZero() || parsedEnd.IsZero() {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid startDate or endDate format"})
		return
	}

	startDate = &parsedStart
	endDate = &parsedEnd

	events, err := business.GetEventsList(ctx, &userInfo, startDate, endDate)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Internal Error", "err": err})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Got events list", "data": events})
}
func LeaveEventController(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	eventUUIdString := chi.URLParam(r, "eventId")

	userInfo, ok := ctx.Value(helpers.UserInfoContextKey).(model.UserInfo)
	if !ok {
		helpers.LogErrorWithContext(ctx, "controllers/LeaveEventController Failed getting user from context")
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{"msg": "Not authorised"})
		return
	}

	if len(eventUUIdString) > 5 && eventUUIdString[:5] == "gcal-" {
		googleEventId := eventUUIdString[5:]
		// For GCal-only events, decline the invitation instead of deleting the event
		err := integrationBusiness.DeclineGoogleCalendarEvent(ctx, googleEventId, &userInfo)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/LeaveEventController Google Calendar decline failed err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to leave Google Calendar event", "err": err})
			return
		}
	} else {
		eventUUID, err := uuid.Parse(eventUUIdString)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/LeaveEventController Parse string to UUid failed err: %+v", err)
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid event id format", "err": err})
			return
		}

		err = business.LeaveEvent(ctx, eventUUID, &userInfo)
		if err != nil {
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Internal Error", "err": err})
			return
		}
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Left event!"})
}
