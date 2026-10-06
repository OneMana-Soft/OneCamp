package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	business "github.com/akashc777/OneCamp/business/Archive"
	"github.com/akashc777/OneCamp/helpers"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const maxRestoreEntityIds = 5000

// checkArchiveRateLimit is a thin wrapper around the registry-backed
// rate limiter so call sites stay readable. Fails open on Redis
// errors (the store helper logs and returns Allowed=true).
func checkArchiveRateLimit(ctx context.Context, action string, userID uuid.UUID, max int) redisStore.RateLimitResult {
	return redisStore.AllowFixedWindow(ctx,
		registry.ArchiveRate,
		[]string{action, userID.String()},
		max,
	)
}

// writeRateLimitExceeded writes a 429 response with a Retry-After header.
func writeRateLimitExceeded(w http.ResponseWriter, retryAfterSeconds int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"error": "rate limit exceeded, try again later"})
}

// HandleGetPolicies returns all archive policies.
func HandleGetPolicies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	policies, err := business.GetArchivePolicies(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"policies": policies})
}

// HandleUpdatePolicy updates a specific archive policy.
func HandleUpdatePolicy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	entityType := chi.URLParam(r, "entityType")
	if entityType == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "entityType is required"})
		return
	}

	var input business.PolicyUpdateInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid request body"})
		return
	}

	err := business.UpdateArchivePolicy(ctx, entityType, input)
	if err != nil {
		if errors.Is(err, business.ErrPolicyNotFound) {
			helpers.WriteJSON(w, http.StatusNotFound, helpers.Envolope{"error": err.Error()})
			return
		}
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Policy updated successfully"})
}

// HandleRunArchiveJob triggers a manual archive job.
func HandleRunArchiveJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, ok := userModel.FromContext(ctx)
	if !ok || userInfo == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Redis-backed rate limit: 5 archive runs per minute per user.
	if res := checkArchiveRateLimit(ctx, "run", userInfo.UserPostgresInfo.Id, 5); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}

	entityType := chi.URLParam(r, "entityType")
	if entityType == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "entityType is required"})
		return
	}

	triggeredBy := &userInfo.UserPostgresInfo.Id
	jobId, err := business.RunArchiveJob(ctx, entityType, triggeredBy)
	if err != nil {
		var alreadyRunning *business.ArchiveAlreadyRunningError
		if errors.As(err, &alreadyRunning) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"error": err.Error(), "code": "already_running"})
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Archive job started", "job_id": jobId})
}

// HandleGetJobs returns archive job history.
func HandleGetJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	jobs, err := business.GetArchiveJobs(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"jobs": jobs})
}

// HandleGetStats returns archive statistics.
func HandleGetStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	stats, err := business.GetArchiveStats(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"stats": stats})
}

// HandleRestore restores archived items.
func HandleRestore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, ok := userModel.FromContext(ctx)
	if !ok || userInfo == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Redis-backed rate limit: 10 restores per minute per user.
	if res := checkArchiveRateLimit(ctx, "restore", userInfo.UserPostgresInfo.Id, 10); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}

	var input struct {
		EntityType string      `json:"entity_type"`
		EntityIds  []uuid.UUID `json:"entity_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid request body"})
		return
	}

	if input.EntityType == "" || len(input.EntityIds) == 0 {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "entity_type and entity_ids are required"})
		return
	}
	if len(input.EntityIds) > maxRestoreEntityIds {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": fmt.Sprintf("entity_ids exceeds maximum of %d per request", maxRestoreEntityIds)})
		return
	}

	count, err := business.RestoreItems(ctx, input.EntityType, input.EntityIds)
	if err != nil {
		var alreadyRunning *business.ArchiveAlreadyRunningError
		if errors.As(err, &alreadyRunning) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"error": err.Error(), "code": "archive_running"})
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "Items restored", "count": count})
}

// HandleUndoArchiveJob reverses the effects of a completed archive job.
func HandleUndoArchiveJob(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	userInfo, ok := userModel.FromContext(ctx)
	if !ok || userInfo == nil {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Redis-backed rate limit: 5 undos per minute per user.
	if res := checkArchiveRateLimit(ctx, "undo", userInfo.UserPostgresInfo.Id, 5); !res.Allowed {
		writeRateLimitExceeded(w, res.RetryAfterSeconds())
		return
	}

	jobIdStr := chi.URLParam(r, "jobId")
	jobId, err := uuid.Parse(jobIdStr)
	if err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"error": "Invalid job ID"})
		return
	}

	count, err := business.UndoArchiveJob(ctx, jobId)
	if err != nil {
		var alreadyRunning *business.ArchiveAlreadyRunningError
		if errors.As(err, &alreadyRunning) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{"error": err.Error(), "code": "archive_running"})
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": fmt.Sprintf("Undone archive job — %d items restored", count), "count": count})
}

// HandleGetRecentArchivedItems returns recently archived items for the restore UI.
func HandleGetRecentArchivedItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	entityType := chi.URLParam(r, "entityType")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	items, total, err := business.GetRecentlyArchivedItems(ctx, entityType, limit, offset)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"error": err.Error()})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"items": items, "total": total})
}
