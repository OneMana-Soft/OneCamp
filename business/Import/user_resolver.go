package business

import (
	"context"
	"fmt"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// UserResolveStats are returned alongside the resolution pass.
type UserResolveStats struct {
	Total      int `json:"total"`
	Reused     int `json:"reused"`
	CreatedExt int `json:"created_external"`
	Bots       int `json:"bots"`
	Skipped    int `json:"skipped"`
}

// runUsersStage walks every SourceUser the provider streams and ensures
// each has a OneCamp users row. Idempotent: re-runs against the same
// import_id are no-ops thanks to the import_id_map upsert, and re-runs
// against the same workspace are no-ops thanks to the workspace map.
//
// Matching policy (in order, first match wins):
//  1. Per-import id_map → no work, just count.
//  2. Workspace id_map (prior import for same provider+workspace).
//  3. Existing OneCamp user with the same email.
//  4. Provision a new external user (synth email if none provided).
func runUsersStage(ctx context.Context, prov importProvider.Provider,
	job *importModels.Job, opts importProvider.JobOptions) error {

	userCh, errCh := prov.IterUsers(ctx, job, opts)

	stats := UserResolveStats{}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errCh:
			if ok && err != nil {
				return err
			}
			// errCh closed without err; continue draining userCh.
			errCh = nil
		case su, ok := <-userCh:
			if !ok {
				patch := mustMarshal(map[string]interface{}{
					"users":             stats,
					"users_resolved_at": time.Now().UTC(),
				})
				_ = importModels.UpdateProgress(ctx, job.Id, patch)
				return nil
			}
			stats.Total++
			if err := resolveOneUser(ctx, prov, job, &su, &stats); err != nil {
				// Resolution should be tolerant of single-user failures.
				// Redact email from the message so privacy-sensitive
				// data isn't forwarded to log aggregators (the per-import
				// id_map metadata still carries the address for operator
				// inspection inside OneCamp's admin UI).
				importModels.LogImportError(ctx, job.Id, nil,
					importModels.EntityUser, su.SourceID,
					importModels.SeverityError, "USER_RESOLVE_FAILED",
					redactEmail(err.Error()), nil)
				stats.Skipped++
			}
		}
	}
}

func resolveOneUser(ctx context.Context, prov importProvider.Provider,
	job *importModels.Job, su *importProvider.SourceUser, stats *UserResolveStats) error {

	if su.SourceID == "" {
		stats.Skipped++
		return nil
	}

	// 1. Per-import map.
	if existing, err := importModels.LookupIdMapping(ctx, job.Id,
		importModels.EntityUser, su.SourceID); err == nil && existing != uuid.Nil {
		stats.Reused++
		return nil
	}

	// 2. Workspace map (cross-import for this provider+workspace).
	if existing, err := importModels.LookupWorkspaceMapping(ctx,
		job.Provider, job.SourceWorkspaceName,
		importModels.EntityUser, su.SourceID); err == nil && existing != uuid.Nil {

		_ = importModels.UpsertIdMappingWithOwnership(ctx, job.Id,
			importModels.EntityUser, su.SourceID, existing, nil,
			mustMarshal(map[string]any{"matched_by": "workspace_map"}),
			false)
		stats.Reused++
		return nil
	}

	// 3. OneCamp by email.
	email := strings.TrimSpace(strings.ToLower(su.Email))
	if email != "" {
		if oc, _ := userBusiness.GetUserByEmailId(ctx, &email); oc != nil {
			_ = importModels.UpsertIdMappingWithOwnership(ctx, job.Id,
				importModels.EntityUser, su.SourceID, oc.Id, nil,
				mustMarshal(map[string]any{
					"matched_by": "email",
					"email":      email,
				}), false)
			_ = importModels.UpsertWorkspaceMapping(ctx,
				job.Provider, job.SourceWorkspaceName,
				importModels.EntityUser, su.SourceID, oc.Id, job.Id)
			stats.Reused++
			return nil
		}
	}

	// 4. Provision an external user.
	emailToUse := email
	displayName := pickDisplayName(su)
	login := su.Login
	if login == "" {
		login = displayName
	}
	if login == "" {
		login = su.SourceID
	}
	// Prefix with provider so the external login can't collide with a
	// real GitHub login (login uniqueness is workspace-scoped on Slack
	// import too — same approach).
	prefixedLogin := job.Provider + "-" + sanitizeLogin(login)

	if emailToUse == "" {
		emailToUse = synthEmail(job.Provider, job.SourceWorkspaceName, su.SourceID)
	}
	// If the synth or real email collides with another user (rare, but
	// real on workspaces with split identities), suffix.
	if existing, _ := userBusiness.GetUserByEmailId(ctx, &emailToUse); existing != nil {
		emailToUse = collisionSuffix(emailToUse, su.SourceID)
	}

	newUser, err := userBusiness.CreateExternalGitHubUser(
		ctx,
		prefixedLogin,
		displayName,
		su.AvatarURL, // raw URL; CreateExternalGitHubUser stores it on profile_object_key
		"",           // no html_url for non-GitHub
		emailToUse,
	)
	if err != nil {
		return fmt.Errorf("create external user: %w", err)
	}

	if err := importModels.UpsertIdMappingWithOwnership(ctx, job.Id,
		importModels.EntityUser, su.SourceID, newUser.Id, nil,
		mustMarshal(map[string]any{
			"matched_by":   "external",
			"email":        emailToUse,
			"display_name": displayName,
			"login":        prefixedLogin,
			"is_bot":       su.IsBot,
		}), true); err != nil {
		return err
	}
	_ = importModels.UpsertWorkspaceMapping(ctx,
		job.Provider, job.SourceWorkspaceName,
		importModels.EntityUser, su.SourceID, newUser.Id, job.Id)

	if su.IsBot {
		stats.Bots++
	} else {
		stats.CreatedExt++
	}
	return nil
}

func pickDisplayName(u *importProvider.SourceUser) string {
	if v := strings.TrimSpace(u.DisplayName); v != "" {
		return v
	}
	if v := strings.TrimSpace(u.Login); v != "" {
		return v
	}
	return u.SourceID
}

func synthEmail(provider, workspaceName, sourceId string) string {
	ws := slugify(workspaceName)
	if ws == "" {
		ws = provider
	}
	return fmt.Sprintf("%s-import-%s-%s@no-reply.local",
		provider, ws, strings.ToLower(sanitizeLogin(sourceId)))
}

func collisionSuffix(email, sourceId string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return email + "+x-" + strings.ToLower(sanitizeLogin(sourceId))
	}
	return email[:at] + "+x-" + strings.ToLower(sanitizeLogin(sourceId)) + email[at:]
}

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

func sanitizeLogin(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'),
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if out == "" {
		return "user"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// resolveUserUUID looks up an already-resolved user. Used by task/comment
// workers that have a source author id and need the OneCamp user.
func resolveUserUUID(ctx context.Context, job *importModels.Job, sourceId string) (uuid.UUID, *userModels.UserInfo) {
	if sourceId == "" {
		return uuid.Nil, nil
	}
	ocUUID, _ := importModels.LookupIdMapping(ctx, job.Id, importModels.EntityUser, sourceId)
	if ocUUID == uuid.Nil {
		return uuid.Nil, nil
	}
	pgUser, err := userBusiness.GetUserByUUID(ctx, ocUUID)
	if err != nil || pgUser == nil {
		return ocUUID, nil
	}
	dgUser, err := userBusiness.GetDgraphUserInfoByUUID(ctx, ocUUID.String())
	if err != nil || dgUser == nil {
		return ocUUID, &userModels.UserInfo{UserPostgresInfo: *pgUser}
	}
	return ocUUID, &userModels.UserInfo{
		UserPostgresInfo: *pgUser,
		UserDgraphInfo:   *dgUser,
	}
}
