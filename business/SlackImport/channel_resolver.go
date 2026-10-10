package business

import (
	"context"
	"fmt"
	"strings"
	"time"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// ChannelResolveStats summarises the channel pass for the FE plan view
// and the live progress.
type ChannelResolveStats struct {
	Total     int      `json:"total"`
	Created   int      `json:"created"`
	Conflicts int      `json:"conflicts"`
	Renamed   []string `json:"renamed,omitempty"` // for warnings
	// IntoGeneral is the name of Slack's #general when it went into the
	// workspace's own #general.
	IntoGeneral string `json:"into_general,omitempty"`
}

// resolveChannels creates a OneCamp channel for each public Slack channel
// in the export, plus each private group (groups.json). DMs and MPIMs
// are handled separately by resolveDMs because they have a different
// shape (no name, multi-participant grouping).
//
// Three-tier dedup, in order:
//
//  1. import_id_map (this run, re-runnable idempotency)
//  2. import_workspace_id_map (prior import of same workspace; cross-run
//     idempotency — a re-export of the same workspace produces zero
//     duplicate channels)
//  3. Existing OneCamp channel by same name (collision suffix
//     "-from-slack" applied)
//
// Tier 2 is the new bit: it means re-importing a workspace 30 days
// later just adds new messages to existing channels rather than
// creating a parallel "general-from-slack-2" universe.
//
// Privacy: Slack `groups.json` entries become OneCamp channels with
// ChannelPrivate=true. The created channel's ACL inherits from the
// import-time member list, so only the resolved Slack members can
// access it post-import.
func resolveChannels(ctx context.Context, importId uuid.UUID, workspaceName string,
	publicChannels, privateGroups []SlackChannel,
	importingUser *userModels.UserInfo, prefix string) (ChannelResolveStats, error) {

	stats := ChannelResolveStats{Total: len(publicChannels) + len(privateGroups)}

	// Shared user cache so 50 channels × 200 members doesn't hit Dgraph
	// 10,000 times. Re-exports of the same workspace see ~0% miss rate.
	userCache := newDgraphUserCache()

	// We process public + private in one loop using a small wrapper so
	// the dedup/conflict logic stays single-source. The only behavioural
	// difference is the ChannelPrivate flag passed to CreateChannel.
	type entry struct {
		ch        *SlackChannel
		isPrivate bool
	}
	all := make([]entry, 0, len(publicChannels)+len(privateGroups))
	for i := range publicChannels {
		all = append(all, entry{ch: &publicChannels[i], isPrivate: false})
	}
	for i := range privateGroups {
		all = append(all, entry{ch: &privateGroups[i], isPrivate: true})
	}

	for _, e := range all {
		sc := e.ch
		if sc.ID == "" || sc.Name == "" {
			continue
		}

		// Tier 1: same import retry.
		if existing, err := importModels.LookupIdMapping(ctx, importId, importModels.EntityChannel, sc.ID); err == nil && existing != uuid.Nil {
			continue
		}

		// Tier 2: a previous import of the same workspace already created
		// this channel. Reuse the OneCamp UUID, record per-import mapping
		// with created_by_this_import=false so rollback won't tombstone it.
		// Membership delta: re-add members (the bulk-add helper is
		// idempotent on the dgraph edge so existing members are no-ops).
		if existing, err := importModels.LookupWorkspaceMapping(ctx, workspaceName, importModels.EntityChannel, sc.ID); err == nil && existing != uuid.Nil {
			if err := importModels.UpsertIdMappingWithOwnership(ctx, importId,
				importModels.EntityChannel, sc.ID, existing, nil,
				mustMarshal(map[string]interface{}{
					"matched_by": "workspace_map",
					"slack_name": sc.Name,
				}),
				false); err != nil {
				return stats, err
			}
			addChannelMembersBulk(ctx, importId, existing, sc.Members, userCache)
			continue
		}

		// Slack's #general goes into the workspace's own #general while that
		// one holds nothing but its welcome post (tidy.go), instead of
		// becoming #general-from-slack beside it. Not created by this import,
		// so a rollback takes away the messages and leaves the channel.
		if sc.IsGeneral && !e.isPrivate {
			if general, ok := seededGeneral(ctx); ok {
				if err := importModels.UpsertIdMappingWithOwnership(ctx, importId,
					importModels.EntityChannel, sc.ID, general, nil,
					mustMarshal(map[string]interface{}{
						"matched_by": "seeded_general",
						"slack_name": sc.Name,
					}),
					false); err != nil {
					return stats, err
				}
				_ = importModels.UpsertWorkspaceMapping(ctx, workspaceName,
					importModels.EntityChannel, sc.ID, general, importId)
				addChannelMembersBulk(ctx, importId, general, sc.Members, userCache)
				stats.IntoGeneral = sc.Name
				continue
			}
		}

		desired := strings.ToLower(strings.TrimSpace(sc.Name))
		if prefix != "" {
			desired = prefix + desired
		}
		desired = sanitizeChannelName(desired)

		// Tier 3: existing OneCamp channel by name (might be from a
		// non-Slack-import source; collision-suffix to avoid stomping it).
		final, conflicted := uniqueChannelName(ctx, desired)
		if conflicted {
			stats.Conflicts++
			if final != desired {
				stats.Renamed = append(stats.Renamed, fmt.Sprintf("%s → %s", desired, final))
			}
		}

		input := &channelAdapter.InputCreateChannel{
			ChannelName:    final,
			ChannelPrivate: e.isPrivate,
		}

		// Backdate the channel to the Slack `created` timestamp so the
		// imported workspace shows historical channels in the right
		// chronological order. CreateChannel honours WithImportTimestamp
		// for both Postgres and Dgraph + OpenSearch.
		ctxBackdated := ctx
		if sc.Created > 0 {
			ctxBackdated = helpers.WithImportTimestamp(ctx, time.Unix(sc.Created, 0))
		}

		err, channelUUID := channelBusiness.CreateChannel(ctxBackdated, input, importingUser)
		if err != nil {
			importModels.LogImportError(ctx, importId, nil,
				importModels.EntityChannel, sc.ID,
				importModels.SeverityError, "CHANNEL_CREATE_FAILED",
				fmt.Sprintf("creating channel %s failed: %v", final, err), nil)
			continue
		}
		stats.Created++

		// Postgres backfill — domain.CreateChannel uses DEFAULT NOW(),
		// so a Slack-time UPDATE here aligns the PG row with the
		// timestamp Dgraph + OpenSearch already received via the
		// backdated context above.
		if sc.Created > 0 {
			if err := backdateChannel(ctx, channelUUID, time.Unix(sc.Created, 0)); err != nil {
				helpers.LogWarnWithContext(ctx,
					"SlackImport could not backdate channel %s: %+v", channelUUID, err)
			}
		}

		md := mustMarshal(map[string]interface{}{
			"slack_name":  sc.Name,
			"final_name":  final,
			"is_archived": sc.IsArchived,
			"is_general":  sc.IsGeneral,
			"is_private":  e.isPrivate,
		})
		if err := importModels.UpsertIdMappingWithOwnership(ctx, importId,
			importModels.EntityChannel, sc.ID, channelUUID, nil, md,
			true /* physically created */); err != nil {
			return stats, err
		}
		// Promote so a subsequent re-import of this workspace finds it.
		_ = importModels.UpsertWorkspaceMapping(ctx, workspaceName,
			importModels.EntityChannel, sc.ID, channelUUID, importId)

		// Wire membership inline (after the channel mapping is recorded
		// so a crash mid-membership can still resume cleanly).
		addChannelMembersBulk(ctx, importId, channelUUID, sc.Members, userCache)

		// Creating it made the importing admin a member and its admin; a
		// private channel they weren't in is handed on and left (tidy.go).
		if e.isPrivate {
			leavePrivateChannel(ctx, importId, channelUUID, sc, importingUser, userCache)
		}
	}

	patch := mustMarshal(map[string]interface{}{"channels": stats})
	_ = importModels.UpdateProgress(ctx, importId, patch)
	return stats, nil
}

// sanitizeChannelName lowercases, replaces unsupported chars with -, and
// trims to a sensible length. Slack allows up to 80 chars; OneCamp's
// schema has no hard cap but indexes work better with shorter names.
func sanitizeChannelName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'), r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ', r == '.', r == '/':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" {
		out = "imported-channel"
	}
	return out
}

// uniqueChannelName tests the desired name against existing channels and
// suffixes if needed. Returns (final, hadConflict).
func uniqueChannelName(ctx context.Context, desired string) (string, bool) {
	if exist, err := channelBusiness.CheckIfChannelExist(ctx, &desired); err == nil && !exist {
		return desired, false
	}
	// Try -from-slack, then -from-slack-2, -3, ... up to 99.
	candidate := desired + "-from-slack"
	if exist, err := channelBusiness.CheckIfChannelExist(ctx, &candidate); err == nil && !exist {
		return candidate, true
	}
	for i := 2; i < 100; i++ {
		candidate = fmt.Sprintf("%s-from-slack-%d", desired, i)
		if exist, err := channelBusiness.CheckIfChannelExist(ctx, &candidate); err == nil && !exist {
			return candidate, true
		}
	}
	// Fallback should never trigger in practice.
	return desired + "-from-slack-x", true
}

// backdateChannel rewrites the channels row created_at/updated_at so the
// imported channel's creation timestamp matches the Slack `created`
// field. CreateChannel uses DEFAULT NOW() in its INSERT, so this UPDATE
// runs immediately after creation to align Postgres with the Dgraph +
// OpenSearch values that came in via the WithImportTimestamp context.
func backdateChannel(ctx context.Context, channelUUID uuid.UUID, t time.Time) error {
	dbCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := importModels.Exec(dbCtx, `
		UPDATE channels SET created_at = $2, updated_at = $2 WHERE id = $1`, channelUUID, t)
	return err
}
