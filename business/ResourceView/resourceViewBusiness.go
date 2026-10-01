package business

// Shared business logic for the "Viewed by" feature (docs + boards). Access
// control is the CALLER's responsibility: recording a view requires the viewer
// to have access to the resource, and listing viewers is restricted to owners
// and editors. This package only records views and resolves the viewer list to
// display info; the resource-specific permission checks live in the doc/board
// business layers that wrap these.

import (
	"context"
	"strconv"

	userDomain "github.com/akashc777/OneCamp/domain/User"
	viewModels "github.com/akashc777/OneCamp/models/postgres/ResourceView"
	redisRegistry "github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// Re-export resource type constants so callers use one source of truth.
const (
	ResourceDoc   = viewModels.ResourceDoc
	ResourceBoard = viewModels.ResourceBoard
)

// ViewerInfo is a resolved viewer for the UI: identity (embedded, so its JSON
// flattens to user_uuid/user_full_name/user_name/user_profile_object_key) plus
// when they last/first viewed.
type ViewerInfo struct {
	userDomain.UserDisplay
	FirstViewedAt string `json:"first_viewed_at"`
	LastViewedAt  string `json:"last_viewed_at"`
}

// ViewersPage is a paginated viewer list with the total distinct-viewer count
// for client-side page math.
type ViewersPage struct {
	Viewers []ViewerInfo `json:"viewers"`
	Total   int          `json:"total"`
	HasMore bool         `json:"has_more"`
}

// RecordView records (deduplicated, throttled) that userUUID viewed a resource.
// The caller must have already verified the user can access the resource.
func RecordView(ctx context.Context, resourceType, resourceUUID, userUUID string) error {
	return viewModels.RecordView(ctx, resourceType, resourceUUID, userUUID)
}

// CachedStateSize returns the last persisted state byte-size for a resource
// (board/doc) and whether it was found. Used on the collab persist path to
// detect a sharp shrink (mass-delete) cheaply, without reading the full prior
// state from the graph on every save. A miss (or Redis being unavailable)
// simply means callers fall back to fetching the prior state.
func CachedStateSize(ctx context.Context, resourceType, resourceID string) (int, bool) {
	v, ok, err := redisStore.GetString(ctx, redisRegistry.CollabStateSize, []string{resourceType, resourceID})
	if err != nil || !ok || v == "" {
		return 0, false
	}
	n, perr := strconv.Atoi(v)
	if perr != nil {
		return 0, false
	}
	return n, true
}

// SetCachedStateSize records the persisted state byte-size for a resource so
// the next save can compare against it. Best-effort.
func SetCachedStateSize(ctx context.Context, resourceType, resourceID string, size int) {
	_ = redisStore.SetString(ctx, redisRegistry.CollabStateSize, []string{resourceType, resourceID}, strconv.Itoa(size))
}

// ListViewersResolved returns one page of the resource's distinct viewers
// (most-recent first) resolved to name/avatar in a SINGLE batch query (no N+1),
// plus the total count for pagination. The caller must have already verified
// the requester may see the viewer list (owner/editor).
func ListViewersResolved(ctx context.Context, resourceType, resourceUUID string, limit, offset int) (*ViewersPage, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	total, err := viewModels.CountViewers(ctx, resourceType, resourceUUID)
	if err != nil {
		return nil, err
	}

	viewers, err := viewModels.ListViewers(ctx, resourceType, resourceUUID, limit, offset)
	if err != nil {
		return nil, err
	}

	// Resolve all viewers' display info in one batch query.
	uuids := make([]string, 0, len(viewers))
	for _, v := range viewers {
		uuids = append(uuids, v.UserUUID)
	}
	displays, err := userDomain.ResolveUserDisplays(ctx, uuids)
	if err != nil {
		displays = map[string]userDomain.UserDisplay{}
	}

	out := make([]ViewerInfo, 0, len(viewers))
	for _, v := range viewers {
		info := ViewerInfo{
			FirstViewedAt: v.FirstViewedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			LastViewedAt:  v.LastViewedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		}
		if d, ok := displays[v.UserUUID]; ok {
			info.UserDisplay = d
		} else {
			// Unresolved (e.g. deactivated) - still show the row with the uuid.
			info.UserDisplay = userDomain.UserDisplay{Uuid: v.UserUUID}
		}
		out = append(out, info)
	}

	return &ViewersPage{
		Viewers: out,
		Total:   total,
		HasMore: offset+len(out) < total,
	}, nil
}
