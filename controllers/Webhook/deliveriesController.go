package controllers

// Admin › Webhooks' deliveries (platform P-API part 2, business/Hooks): what
// was sent to each webhook and how it went, a delivery sent again, and a
// webhook paused and resumed. Under the admin group, which a session reaches
// and an API token never does; each change is in the audit log.

import (
	"errors"
	"net/http"
	"strconv"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	hooksBusiness "github.com/akashc777/OneCamp/business/Hooks"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func webhookIDOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "webhookId"))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a webhook's id."})
		return uuid.Nil, false
	}
	return id, true
}

// HandleGetDeliveries is GET /webhooks/{webhookId}/deliveries?state=&cursor=&limit=:
// a page of the webhook's deliveries, newest first.
func HandleGetDeliveries(w http.ResponseWriter, r *http.Request) {
	id, ok := webhookIDOf(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	page, err := hooksBusiness.Deliveries(r.Context(), id, q.Get("state"), q.Get("cursor"), helpers.LimitParam(q, 25, 100))
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": page})
}

// HandleGetDeliveryHealth is GET /webhook-health: how every outgoing webhook's
// deliveries stand (paused, failing, waiting, given up on, last delivered).
func HandleGetDeliveryHealth(w http.ResponseWriter, r *http.Request) {
	health, err := hooksBusiness.Health(r.Context())
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/HandleGetDeliveryHealth: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't read how the webhooks' deliveries stand. Try again in a moment."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": health})
}

// HandleReplayDelivery is POST /webhooks/{webhookId}/deliveries/{deliveryId}/replay:
// the delivery sent again from its first try, with the same body and event id.
func HandleReplayDelivery(w http.ResponseWriter, r *http.Request) {
	id, ok := webhookIDOf(w, r)
	if !ok {
		return
	}
	deliveryID, err := strconv.ParseInt(chi.URLParam(r, "deliveryId"), 10, 64)
	if err != nil || deliveryID < 1 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "That isn't a delivery's id."})
		return
	}
	if err := hooksBusiness.Replay(r.Context(), id, deliveryID); err != nil {
		if errors.Is(err, hooksBusiness.ErrNotFound) {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such delivery, or it's being sent right now."})
			return
		}
		helpers.LogErrorWithContext(r.Context(), "controllers/HandleReplayDelivery: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't send it again. Try again in a moment."})
		return
	}
	auditBusiness.Record(r, "webhook.delivery_replayed", auditBusiness.CategoryIntegration, "Sent a webhook delivery again",
		map[string]interface{}{"webhook_id": id.String(), "delivery_id": deliveryID})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Sending it again."})
}

// HandleReplayFailed is POST /webhooks/{webhookId}/deliveries/replay-failed:
// every failed delivery sent again from its first try (after an outage), each
// with its own body and event id; answers how many go.
func HandleReplayFailed(w http.ResponseWriter, r *http.Request) {
	id, ok := webhookIDOf(w, r)
	if !ok {
		return
	}
	n, err := hooksBusiness.ReplayFailed(r.Context(), id)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/HandleReplayFailed: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't send them again. Try again in a moment."})
		return
	}
	if n > 0 {
		auditBusiness.Record(r, "webhook.deliveries_replayed", auditBusiness.CategoryIntegration, "Sent a webhook's failed deliveries again",
			map[string]interface{}{"webhook_id": id.String(), "count": n})
	}
	msg := "Nothing had failed."
	if n > 0 {
		msg = "Sending them again."
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": msg, "data": map[string]any{"count": n}})
}

// HandlePauseWebhook is POST /webhooks/{webhookId}/pause: its deliveries wait
// until it's resumed.
func HandlePauseWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := webhookIDOf(w, r)
	if !ok {
		return
	}
	if err := hooksBusiness.Pause(r.Context(), id); err != nil {
		if errors.Is(err, hooksBusiness.ErrNotFound) {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such webhook; it may have been removed."})
			return
		}
		helpers.LogErrorWithContext(r.Context(), "controllers/HandlePauseWebhook: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't pause it. Try again in a moment."})
		return
	}
	auditBusiness.Record(r, "webhook.paused", auditBusiness.CategoryIntegration, "Paused a webhook",
		map[string]interface{}{"webhook_id": id.String()})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Paused. What it would send waits until you resume it."})
}

// HandleResumeWebhook is POST /webhooks/{webhookId}/resume: its waiting
// deliveries go now. An admin who resumes a webhook whose creator left, or
// can't read what it's set up for, takes it over if they can read it.
func HandleResumeWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := webhookIDOf(w, r)
	if !ok {
		return
	}
	by, _, _ := auditBusiness.ActorFromContext(r.Context())
	tookOver, err := hooksBusiness.Resume(r.Context(), id, by)
	if err != nil {
		if errors.Is(err, hooksBusiness.ErrNotFound) {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no such webhook; it may have been removed."})
			return
		}
		if errors.Is(err, hooksBusiness.ErrCantTakeOver) {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "Its creator can't send it any more, and you can't read the channel or project it's set up for either, so you can't take it over. Someone who can may resume it."})
			return
		}
		helpers.LogErrorWithContext(r.Context(), "controllers/HandleResumeWebhook: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "Couldn't resume it. Try again in a moment."})
		return
	}
	summary, msg := "Resumed a webhook", "Resumed. What was waiting is on its way."
	if tookOver {
		summary, msg = "Resumed a webhook and took it over", "Resumed, and it's yours now: its creator couldn't send it any more. It sends only what you can read."
	}
	auditBusiness.Record(r, "webhook.resumed", auditBusiness.CategoryIntegration, summary,
		map[string]interface{}{"webhook_id": id.String(), "took_over": tookOver})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": msg, "took_over": tookOver})
}
