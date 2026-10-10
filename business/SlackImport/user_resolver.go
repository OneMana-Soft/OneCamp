package business

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// UserResolveStats are returned alongside the resolution pass so the
// orchestrator can patch them into the job progress for the FE.
type UserResolveStats struct {
	Total      int `json:"total"`
	Reused     int `json:"reused"` // matched by email
	CreatedExt int `json:"created_external"`
	Bots       int `json:"bots"`
	Skipped    int `json:"skipped"`
}

// resolveUsers walks every Slack user in the export and ensures each has
// a OneCamp users row. Idempotent: re-runs against the same import_id are
// no-ops thanks to the import_id_map upsert.
//
// Matching policy (in order, first match wins):
//
//  1. Real email present and a OneCamp user exists with that email →
//     reuse that user, no profile mutation.
//  2. Real email present, no OneCamp match → create as external user.
//  3. No real email → synth email "slack-import-<workspace>-<U_id>@no-reply.local"
//     and create as external user. Bots, deactivated users, and
//     email-less workspace members all land here.
//
// Two Slack users sharing a real email (rare but observed in old
// workspaces) is resolved by suffixing "+slack-<U_id>" before the @ on
// the second one's synth so the unique constraint holds.
func resolveUsers(ctx context.Context, importId uuid.UUID, workspaceName string, users []SlackUser, importingAdminUUID uuid.UUID) (UserResolveStats, error) {
	stats := UserResolveStats{}
	stats.Total = len(users)

	// Pre-pass: every user gets either an existing OneCamp uuid or a
	// freshly-allocated one. We don't INSERT users in bulk because the
	// existing CreateExternalGitHubUser path already creates a Dgraph
	// user too — that's the price of consistency with the rest of the
	// codebase. For tens of thousands of users this loop is the bound,
	// so we keep it simple.
	for i := range users {
		su := &users[i]

		// Skip the "USLACKBOT" sentinel and any deleted users with no
		// content attribution we can't make. They're cheap to keep
		// because they may be referenced by old messages, but bot_message
		// records have their own profile so we don't strictly need them.
		// We still create a placeholder if ever referenced; that happens
		// lazily inside the message worker.
		if su.ID == "" {
			stats.Skipped++
			continue
		}

		// 1. Already mapped within this import? (re-runnable per-import idempotency)
		if existing, err := importModels.LookupIdMapping(ctx, importId, importModels.EntityUser, su.ID); err == nil && existing != uuid.Nil {
			stats.Reused++
			continue
		}

		// 2. Already imported by a PRIOR import of this workspace?
		// Cross-import dedup: a re-export of the same workspace points
		// at the same Slack user ids; we reuse the OneCamp user we
		// already provisioned and only record the per-import mapping.
		if existing, err := importModels.LookupWorkspaceMapping(ctx, workspaceName, importModels.EntityUser, su.ID); err == nil && existing != uuid.Nil {
			if err := importModels.UpsertIdMappingWithOwnership(ctx, importId,
				importModels.EntityUser, su.ID, existing, nil,
				mustMarshal(map[string]interface{}{"matched_by": "workspace_map"}),
				false /* not created by this import */); err != nil {
				return stats, err
			}
			stats.Reused++
			continue
		}

		email := strings.TrimSpace(strings.ToLower(su.Profile.Email))
		var ocUser *userModels.User

		if email != "" {
			// 3. Match by email against any OneCamp user.
			ocUser, _ = userBusiness.GetUserByEmailId(ctx, &email)
		}

		if ocUser != nil {
			// Reuse: do not mutate profile, do not invite, just record map.
			if err := importModels.UpsertIdMappingWithOwnership(ctx, importId, importModels.EntityUser, su.ID,
				ocUser.Id, nil, mustMarshal(map[string]interface{}{
					"matched_by": "email",
					"email":      email,
				}), false /* matched, not created */); err != nil {
				return stats, err
			}
			// Promote into the workspace map so the next re-import skips it.
			_ = importModels.UpsertWorkspaceMapping(ctx, workspaceName,
				importModels.EntityUser, su.ID, ocUser.Id, importId)
			stats.Reused++
			continue
		}

		// 3. Provision an external user.
		emailToUse := email
		displayName := pickDisplayName(su)
		if emailToUse == "" {
			// Synth email so the unique index + FK both hold.
			emailToUse = synthEmail(workspaceName, su.ID)
		} else {
			// Email exists in Slack but not in OneCamp; check the synth
			// space wasn't already taken (rare collision with an earlier
			// import). If so, suffix.
			if existing, _ := userBusiness.GetUserByEmailId(ctx, &emailToUse); existing != nil {
				emailToUse = collisionSuffix(emailToUse, su.ID)
			}
		}

		// Upload the avatar to MinIO so the imported user keeps a
		// working profile picture after the Slack CDN URL expires
		// (~90 days). On any failure we fall through to creating the
		// user without a profile picture; the FE renders initials.
		//
		// avatarKey is the attachment UUID stored in DgraphUser.ProfileKey
		// — that's the contract with GetSignedProfileURL (it looks up
		// the attachment row and presigns its obj_key).
		avatarKey := ""
		if rawURL := pickAvatarURL(su); rawURL != "" {
			key, err := uploadSlackAvatar(ctx, importId, importingAdminUUID, su.ID, rawURL)
			if err != nil {
				helpers.LogWarnWithContext(ctx,
					"SlackImport.resolveUsers avatar upload failed slackId=%s err=%+v",
					su.ID, err)
				importModels.LogImportError(ctx, importId, nil,
					importModels.EntityUser, su.ID,
					importModels.SeverityWarning, "AVATAR_FETCH_FAILED",
					fmt.Sprintf("avatar fetch failed: %v", err),
					mustMarshal(map[string]interface{}{"url": rawURL}))
			} else {
				avatarKey = key
			}
		}

		// Reuse the GitHub-external pathway. We pass the Slack-side
		// metadata via the github_* fields because the schema has them
		// today. A future migration can rename these to generic
		// external_* columns; until then this keeps the import working
		// without yet another schema change.
		//
		// avatarKey (the attachment UUID) is passed instead of the raw
		// Slack URL so DgraphUser.ProfileKey holds something stable.
		newUser, err := userBusiness.CreateExternalGitHubUser(
			ctx,
			"slack-"+su.Name, // login (must be unique-ish; prefixed so it can't collide with a real GH login)
			displayName,
			avatarKey,
			"", // htmlURL — not applicable to Slack
			emailToUse,
		)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"SlackImport.resolveUsers failed to create external user slackId=%s err=%+v",
				su.ID, err)
			importModels.LogImportError(ctx, importId, nil,
				importModels.EntityUser, su.ID,
				importModels.SeverityError, "USER_CREATE_FAILED",
				fmt.Sprintf("could not create external user: %v", err), nil)
			stats.Skipped++
			continue
		}

		if err := importModels.UpsertIdMappingWithOwnership(ctx, importId, importModels.EntityUser, su.ID,
			newUser.Id, nil, mustMarshal(map[string]interface{}{
				"matched_by":   "external",
				"email":        emailToUse,
				"display_name": displayName,
				"is_bot":       su.IsBot,
				"deleted":      su.Deleted,
			}), true /* this import physically created the user */); err != nil {
			return stats, err
		}
		// Promote into the workspace map so a future re-import skips this user.
		_ = importModels.UpsertWorkspaceMapping(ctx, workspaceName,
			importModels.EntityUser, su.ID, newUser.Id, importId)

		if su.IsBot {
			stats.Bots++
		} else {
			stats.CreatedExt++
		}
	}

	// Patch progress.
	patch := mustMarshal(map[string]interface{}{
		"users":             stats,
		"users_resolved_at": time.Now().UTC(),
	})
	_ = importModels.UpdateProgress(ctx, importId, patch)
	return stats, nil
}

// pickDisplayName returns the most human-friendly name from a Slack user
// profile. Slack populates these inconsistently across exports.
func pickDisplayName(u *SlackUser) string {
	if v := strings.TrimSpace(u.Profile.DisplayName); v != "" {
		return v
	}
	if v := strings.TrimSpace(u.Profile.RealName); v != "" {
		return v
	}
	if v := strings.TrimSpace(u.RealName); v != "" {
		return v
	}
	return u.Name
}

// pickAvatarURL prefers the larger image_512 over the original. The
// original may be huge (>5MB); 512 is the safer balance for our UI.
func pickAvatarURL(u *SlackUser) string {
	if u.Profile.Image512 != "" {
		return u.Profile.Image512
	}
	return u.Profile.ImageOriginal
}

// synthEmail produces a deterministic placeholder email. The shape is
// matched by the FE's isExternalUser() check (.no-reply.local domain).
func synthEmail(workspaceName, slackUserId string) string {
	ws := slugify(workspaceName)
	if ws == "" {
		ws = "slack"
	}
	return fmt.Sprintf("slack-import-%s-%s@no-reply.local", ws, strings.ToLower(slackUserId))
}

// collisionSuffix is used when the Slack-supplied email already maps to
// an OneCamp user — usually because that real user had a different
// display name on Slack and we want to keep both records distinct.
func collisionSuffix(email, slackUserId string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return email + "+slack-" + strings.ToLower(slackUserId)
	}
	return email[:at] + "+slack-" + strings.ToLower(slackUserId) + email[at:]
}

// slugify produces a-z0-9- only string safe for the synth email local-part.
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case !prevDash && b.Len() > 0:
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// mustMarshal panics only if json.Marshal of a struct of basic types
// fails, which is a programmer error. We use it for tiny inline blobs.
func mustMarshal(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Returning empty-object ensures callers don't have to
		// special-case nil; the JSONB column accepts {}.
		return json.RawMessage(`{}`)
	}
	return b
}
