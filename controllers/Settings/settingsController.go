// Package controllers (Settings) exposes the admin workspace-settings surface
// and a small public client-config endpoint.
//
//   - GET  /admin/settings        → redacted admin view (admin-gated)
//   - POST /admin/settings        → save settings (admin-gated)
//   - GET  /config/client         → client-facing config (auth'd users): the
//     upload limit so the composer can validate
//     a file BEFORE uploading and show a precise
//     message instead of a failed request.
package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	business "github.com/akashc777/OneCamp/business/Settings"
	transcriptionBusiness "github.com/akashc777/OneCamp/business/Transcription"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
)

// GetSettings handles GET /admin/settings.
func GetSettings(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.GetStatus()})
}

// UpdateSettings handles POST /admin/settings. Partial: omitted fields are left
// unchanged; secret fields follow omit=keep / ""=clear / value=set.
func UpdateSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var body struct {
		UploadLimitMB *int64    `json:"upload_limit_mb,omitempty"`
		AllowedUsers  *[]string `json:"allowed_users,omitempty"`
		ResendAPIKey  *string   `json:"resend_api_key,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}

	if err := business.Save(business.SaveInput{
		UploadLimitMB: body.UploadLimitMB,
		AllowedUsers:  body.AllowedUsers,
		ResendAPIKey:  body.ResendAPIKey,
	}); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Settings/UpdateSettings err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to save settings"})
		return
	}

	// Audit each changed field (redacted: the Resend key value is never logged).
	if body.UploadLimitMB != nil {
		auditBusiness.Record(r, "settings.upload_limit", auditBusiness.CategorySettings,
			"Changed upload limit", map[string]interface{}{"upload_limit_mb": *body.UploadLimitMB})
	}
	if body.AllowedUsers != nil {
		auditBusiness.Record(r, "settings.allowed_users", auditBusiness.CategorySettings,
			"Updated sign-up allow-list", map[string]interface{}{"count": len(*body.AllowedUsers)})
	}
	if body.ResendAPIKey != nil {
		auditBusiness.Record(r, "settings.resend_api_key", auditBusiness.CategorySecurity,
			auditBusiness.SecretChangeSummary("Resend API key", body.ResendAPIKey), nil)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": business.GetStatus()})
}

// GetClientConfig handles GET /config/client — non-admin, authenticated. Returns
// only the bits the client UI needs to behave well (currently the upload
// limit). Kept deliberately minimal so it's safe to expose to every user.
func GetClientConfig(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"data": map[string]interface{}{
			"upload_limit_mb":    business.UploadLimitMB(),
			"upload_limit_bytes": business.UploadLimitBytes(),
			// Runtime transcription mode (frontend | backend | off). The FE
			// FrontendTranscriber reads this instead of a build-time env var,
			// so an admin can switch transcription on/off without a rebuild.
			"transcription_mode": transcriptionBusiness.Mode(),
			// Which OPTIONAL subsystems this server actually has, so the client can
			// hide what it cannot reach. A button that calls an endpoint which is not
			// there is worse than no button, and two situations produce exactly that:
			// the AI-free v1 edition has no AI routes at all, and on v2 an operator can
			// turn AI off.
			//
			// Read from the registry rather than asked of each subsystem directly. This
			// endpoint exists in BOTH editions, so it must not import a package that
			// only one of them has: v1 removes the AI packages, nothing registers, and
			// the map simply has no "ai" key. Any future optional subsystem appears here
			// by registering, with no change to this handler.
			"features": helpers.FeatureStatus(),
			// False on every freshly provisioned workspace, until an admin adds a
			// sending key. The client uses it to warn, because nothing else will:
			// password reset and invitations both fail silently by design.
			"email_enabled": business.EmailEnabled(),
		},
	})
}

// GetAuditLog handles GET /admin/audit-log — the admin config audit viewer.
// Query: ?category=<one of the returned categories> &initiator=<kind|unattended> &limit &offset.
//
// The response carries the CATEGORY LIST as well as the entries, so the client
// renders one filter per category the server actually uses instead of keeping its
// own copy. That copy had already drifted: the agent category was added later and
// the UI never learned it, leaving every agent and MCP entry — including refusals —
// visible only under "all". Serving the list makes a new category appear in the UI
// with nothing to remember.
func GetAuditLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	category := r.URL.Query().Get("category")
	// "unattended" is the filter an auditor reaches for: what ran on somebody's
	// authority while they were away. A kind narrows to one cause.
	initiator := strings.TrimSpace(r.URL.Query().Get("initiator"))
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)

	entries, err := auditBusiness.ListWhere(ctx, category, initiator, limit, offset)
	if err != nil {
		// An unknown initiator is the caller's error, and the message says which.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"entries":    entries,
		"categories": auditBusiness.AllCategories(),
		// The vocabulary, served so the client renders what the server writes
		// rather than keeping a copy that drifts, the same reason categories
		// are served.
		"initiators": auditBusiness.InitiatorKinds(),
	}})
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// parseAuditExportRange reads optional `from` and `to` RFC3339 timestamps for
// the audit export. An absent value means "unbounded on that side". `to` is
// normalised to the end of the day when it contains no time component, so a
// date like "2026-08-19" includes the whole day rather than only 00:00:00.
func parseAuditExportRange(r *http.Request) (from, to *time.Time, err error) {
	parse := func(key string, endOfDay bool) (*time.Time, error) {
		raw := strings.TrimSpace(r.URL.Query().Get(key))
		if raw == "" {
			return nil, nil
		}
		// Accept either a full RFC3339 timestamp or a calendar date.
		for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02"} {
			if t, perr := time.Parse(layout, raw); perr == nil {
				if layout == "2006-01-02" && endOfDay {
					t = t.Add(24*time.Hour - time.Nanosecond)
				}
				return &t, nil
			}
		}
		return nil, fmt.Errorf("%q must be an RFC3339 timestamp or YYYY-MM-DD date", key)
	}

	from, err = parse("from", false)
	if err != nil {
		return nil, nil, err
	}
	to, err = parse("to", true)
	if err != nil {
		return nil, nil, err
	}
	if from != nil && to != nil && to.Before(*from) {
		return nil, nil, fmt.Errorf("'to' must be after 'from'")
	}
	return from, to, nil
}

// VerifyAuditLog handles GET /admin/audit-log/verify — recomputes the audit
// hash chain and reports whether it is intact (and the first divergence if not).
// This is the tamper-evidence an auditor relies on.
// verifyRecentWindow is how much of the chain a default Verify recomputes.
//
// Big enough that the answer says something about real history, small enough to
// stay a request rather than a report.
const verifyRecentWindow = 500

// VerifyAuditLog handles GET /admin/audit-log/verify?scope=recent|full.
//
// RECENT BY DEFAULT, and that is a deliberate change from walking everything.
// The log only grows, so a full recomputation is instant on a fresh install and
// becomes a slow query and then a gateway timeout on a workspace that has been
// running a year -- at which point the button that proves the log is intact is
// the one thing an auditor cannot use. A bounded check answers immediately and
// the full walk is one explicit click away.
//
// The result says which of the two it did. "The last 500 entries verify" and
// "the log has not been altered" are different claims: a window seeds from its
// earliest row's stored hash rather than from the first entry ever written, so it
// proves the links inside itself and takes that one value on trust. Reporting the
// smaller claim as the larger one would be the exact failure this subsystem
// exists to prevent, so Partial and FromSeq travel with the answer.
func VerifyAuditLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var (
		res *auditModel.VerifyResult
		err error
	)
	// Anything other than an explicit "full" is treated as recent, so a typo
	// costs a reader speed rather than silently starting a table scan.
	if r.URL.Query().Get("scope") == "full" {
		res, err = auditBusiness.Verify(ctx)
	} else {
		res, err = auditBusiness.VerifyRecent(ctx, verifyRecentWindow)
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/VerifyAuditLog failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to verify audit log"})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": res})
}

// ExportAuditLog handles GET /admin/audit-log/export?format=csv|json&category=...&from=RFC3339&to=RFC3339
// — downloads the audit entries (chain order) including the per-row hash, so the
// export is independently verifiable. CSV by default.
// GetRetentionPolicy handles GET /admin/retention
//
// Returns the window, the floor, and which stores it applies to, because a
// number on its own is not a policy somebody can reason about: a reviewer
// finding a record with no content needs to know what was configured and what it
// covered.
func GetRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	stores := []string{}
	for _, s := range helpers.RetentionSweepers() {
		stores = append(stores, s.Name)
	}
	days := business.RetentionDays()
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"window_days":        days,
		"keeps_everything":   days <= 0,
		"minimum_days_floor": business.MinRetentionDays,
		"swept_stores":       stores,
	}})
}

// SetRetentionPolicy handles POST /admin/retention
//
// Changing how long a workspace keeps its records is exactly the kind of
// administrative act the audit log exists for, and until now it was a deploy and
// left no trace at all.
func SetRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WindowDays int `json:"window_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request"})
		return
	}
	if err := business.SetRetentionDays(req.WindowDays); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to save the retention policy"})
		return
	}

	// Read back rather than echo: the floor may have raised the value, and the
	// person is entitled to see what was actually stored rather than what they
	// typed.
	applied := business.RetentionDays()
	summary := "Set the retention window to keep everything"
	if applied > 0 {
		summary = fmt.Sprintf("Set the retention window to %d days", applied)
	}
	auditBusiness.Record(r, "settings.retention", auditBusiness.CategorySecurity, summary,
		map[string]interface{}{"requested_days": req.WindowDays, "applied_days": applied})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{
		"window_days":        applied,
		"keeps_everything":   applied <= 0,
		"minimum_days_floor": business.MinRetentionDays,
	}})
}

// ExportEvidencePack returns the assembled evidence pack for a window.
//
// Separate from ExportAuditLog rather than another format on it, because they
// are different documents for different readers. The export is the log, for
// someone who wants the rows. The pack is an argument, for someone deciding
// whether to trust the system: it carries the chain recomputation, what each
// agent was told, a manifest fingerprinting every section, and a plain
// statement of what it does not prove.
//
// The export itself is an audited administrative action. Who asked for the
// evidence, and when, is exactly the kind of fact the evidence exists to record.
// ListEvidenceReceipts handles GET /admin/audit-log/receipts.
//
// What each completed month's pack said, at the time it said it. A pack can
// always be regenerated; a receipt is the only record of what the earlier one
// contained, taken before retention redacted the rows it was computed over.
func ListEvidenceReceipts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	receipts, err := auditBusiness.ListReceipts(ctx, 24)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ListEvidenceReceipts err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg": "Could not read the evidence receipts",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"status": "success", "data": receipts})
}

func ExportEvidencePack(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	from, to, parseErr := parseAuditExportRange(r)
	if parseErr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": parseErr.Error()})
		return
	}
	// An unbounded pack is a denial-of-service on the reviewer as much as on the
	// database, so both ends are always concrete. Ninety days back by default,
	// which covers a quarter, the unit audits are usually scoped in.
	now := time.Now().UTC()
	fromT := now.AddDate(0, 0, -90)
	toT := now
	if from != nil {
		fromT = *from
	}
	if to != nil {
		toT = *to
	}
	if !toT.After(fromT) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "The end of the range must be after the start."})
		return
	}

	// The same resolver the audit log uses, so the pack and the log cannot
	// disagree about who asked for it.
	actor := ""
	if _, email, ok := auditBusiness.ActorFromContext(ctx); ok {
		actor = email
	}

	pack, err := auditBusiness.BuildEvidencePack(ctx, fromT, toT, actor)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ExportEvidencePack failed: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to build the evidence pack"})
		return
	}

	// Recorded before it is served. Who took the evidence out of the building,
	// and for which window, is exactly the kind of fact the evidence exists to
	// capture, and a pack that did not record its own export would have a hole
	// in it shaped like itself.
	auditBusiness.Record(r, "audit.evidence_pack.export", auditBusiness.CategorySecurity,
		fmt.Sprintf("Exported an evidence pack covering %s to %s",
			fromT.Format("2006-01-02"), toT.Format("2006-01-02")),
		map[string]interface{}{
			"from":             fromT.Format(time.RFC3339),
			"to":               toT.Format(time.RFC3339),
			"pack_fingerprint": pack.Integrity.PackFingerprint,
			"sections":         len(pack.Integrity.Manifest),
		})

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="onecamp-evidence-%s-to-%s.json"`,
			fromT.Format("2006-01-02"), toT.Format("2006-01-02")))
	enc := json.NewEncoder(w)
	// Indented, because a person reads this one.
	enc.SetIndent("", "  ")
	_ = enc.Encode(pack)
}

func ExportAuditLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	category := r.URL.Query().Get("category")
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))

	from, to, parseErr := parseAuditExportRange(r)
	if parseErr != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": parseErr.Error()})
		return
	}

	entries, err := auditBusiness.ExportEntries(ctx, category, from, to)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to export audit log"})
		return
	}

	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="audit-log.json"`)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"entries": entries})
		return
	}

	csvBytes, cerr := auditBusiness.EntriesToCSV(entries)
	if cerr != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to render CSV"})
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="audit-log.csv"`)
	_, _ = w.Write(csvBytes)
}

// ---- Push notifications ----

// GetPushConfig handles GET /admin/push
//
// Returns which project is configured and whether the client actually loaded,
// and never the credential. A key that can be read back is a key that leaves in
// a screenshot or a support ticket; an admin who needs a different one pastes a
// different one.
func GetPushConfig(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"data": business.PushStatus(firebaseInit.Available()),
	})
}

// SetPushConfig handles POST /admin/push
//
// Validates, stores encrypted, and reloads in place, so push starts working
// without a restart. Reloading is the point: the previous arrangement needed a
// file mounted into the container, which meant enabling push was a deploy, which
// meant on this deployment it never happened at all.
func SetPushConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CredentialJSON string `json:"credential_json"`
	}
	// Bounded before decode. The body is a credential document, not a file
	// upload, and the validator's own ceiling is far below this.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request"})
		return
	}

	projectID, clientEmail, err := business.SetFirebaseCredential(req.CredentialJSON)
	if err != nil {
		// The validator's messages name what is wrong with the paste and are
		// safe to show: they describe shape, never content.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": err.Error()})
		return
	}

	if rerr := reloadPush(); rerr != nil {
		// Stored but not live. Reporting success here would leave an admin
		// believing push works, which is the exact failure being fixed.
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg": "Saved, but Firebase rejected the credential: " + rerr.Error(),
		})
		return
	}

	auditBusiness.Record(r, "settings.push.configure", auditBusiness.CategorySecurity,
		fmt.Sprintf("Configured push notifications for Firebase project %s", projectID),
		map[string]interface{}{"project_id": projectID, "client_email": clientEmail})

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"data": business.PushStatus(firebaseInit.Available()),
	})
}

// DeletePushConfig handles DELETE /admin/push, turning push off.
func DeletePushConfig(w http.ResponseWriter, r *http.Request) {
	if err := business.ClearFirebaseCredential(); err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to remove the credential"})
		return
	}
	// Best effort: the credential is gone from storage either way, and leaving a
	// live client running against a key an admin just removed would be worse
	// than a log line.
	_ = reloadPush()

	auditBusiness.Record(r, "settings.push.remove", auditBusiness.CategorySecurity,
		"Removed the push notification credential", nil)

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"data": business.PushStatus(firebaseInit.Available()),
	})
}

// reloadPush re-initialises Firebase from whatever is now configured.
//
// One function rather than the same three lines in two handlers, because the
// resolution order (setting, then mounted file) has to be identical at boot and
// at every change, or the running client and the admin screen describe
// different keys.
func reloadPush() error {
	return firebaseInit.ConnectFirebase(&firebaseInit.FirebaseAppConfigStruct{
		FirebaseCredJSON: business.FirebaseCredentialJSON(),
	})
}

// SetReadReceiptsPolicy POST /admin/read-receipts {enabled}: whether people
// may see who has read their DMs and group chats. Each person can still turn
// their own off.
func SetReadReceiptsPolicy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Invalid request body"})
		return
	}
	if err := business.SetReadReceiptsEnabled(body.Enabled); err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/SetReadReceiptsPolicy err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Couldn't save the read receipts setting."})
		return
	}
	summary := "Turned read receipts off for the workspace"
	if body.Enabled {
		summary = "Turned read receipts on for the workspace"
	}
	auditBusiness.Record(r, "settings.read_receipts", auditBusiness.CategorySettings, summary, map[string]interface{}{"enabled": body.Enabled})
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]interface{}{"read_receipts_enabled": body.Enabled}})
}
