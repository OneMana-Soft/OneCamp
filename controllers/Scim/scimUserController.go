package controllers

// HTTP surface for SCIM 2.0 (RFC 7643/7644): /scim/v2.
//
// Authenticated by a workspace provisioning credential, NOT a session cookie and NOT an /v1 api token —
// see middleware/scimToken.go for why borrowing the latter would make a directory lose access the moment
// it offboarded whoever configured it.
//
// Everything here answers with a SCIM envelope, including failures. An identity provider parses the
// error body to decide whether to retry, skip or surface the problem to an administrator; handed this
// codebase's usual {"msg": ...} shape it sees an unrecognised object, and reports a generic sync failure
// with no detail on the one screen an operator will actually look at.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	scimBusiness "github.com/akashc777/OneCamp/business/Scim"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	"github.com/go-chi/chi/v5"
)

// scimContentType is the media type RFC 7644 §3.1 defines for SCIM payloads.
const scimContentType = "application/scim+json"

// maxSCIMBody caps a decoded SCIM request. A user resource is a small object; the route group also
// carries a BodyLimit, and this bounds the decoder itself.
const maxSCIMBody = 1 << 20

// writeSCIM writes a SCIM response.
//
// NOT helpers.WriteJSON, and not by choice. That function sets Content-Type: application/json AFTER
// applying any caller-supplied headers, so the SCIM media type cannot be produced through it — a
// deliberate chokepoint doing its job, just not the one needed here. Written directly rather than by
// changing WriteJSON's header handling, because ~790 handlers depend on that behaviour and none of them
// should change to let one surface set a different type.
func writeSCIM(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", scimContentType)
	out, err := json.Marshal(body)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/writeSCIM Failed to marshal err: %+v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	if _, err := w.Write(out); err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/writeSCIM Failed to write err: %+v", err)
	}
}

// writeSCIMError writes the SCIM error envelope.
//
// `detail` is always a sentence this codebase authored. Never err.Error(): a *pq.Error's message names
// the constraint, the table and the column, and here it would be sent to a third-party service that logs
// it into an administration console outside this deployment.
func writeSCIMError(w http.ResponseWriter, r *http.Request, status int, scimType string, detail string) {
	writeSCIM(w, r, status, scimBusiness.ScimError{
		Schemas:  []string{scimBusiness.SchemaError},
		Detail:   detail,
		Status:   strconv.Itoa(status),
		ScimType: scimType,
	})
}

// scimBaseURL reconstructs this service's own /scim/v2 URL for meta.location.
//
// Derived from the REQUEST rather than an environment variable, because location must be a URL the caller
// can call back and only the caller knows which host it reached. OneCamp is deployed behind a reverse
// proxy that rewrites Host, so the forwarded headers are the accurate source.
//
// Trusting a client-settable header is safe HERE and would not be elsewhere: the value is echoed to the
// caller that supplied it and is used for nothing else — no redirect, no signature, no authorization
// decision — so poisoning it corrupts only the poisoner's own response.
func scimBaseURL(r *http.Request) string {
	scheme := "https"
	if proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = strings.ToLower(strings.Split(proto, ",")[0])
	} else if r.TLS == nil {
		// No proxy header and no TLS: a direct local request, which is only ever development.
		scheme = "http"
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	host = strings.TrimSpace(strings.Split(host, ",")[0])
	return scheme + "://" + host + "/scim/v2"
}

// scimFailure maps a business error onto the SCIM envelope.
//
// One place, so a route cannot answer a conflict with a 500 by forgetting a case. The default is 500 with
// a generic sentence: an error this function does not recognise is by definition not one we have decided
// how to describe, and inventing a specific reason for it would mislead.
func scimFailure(w http.ResponseWriter, r *http.Request, err error) {
	// errors.Is, not a message comparison. The sentinels' text IS shown to the caller, so matching on it
	// would tie the wire status code to the wording of a user-facing sentence — reword the sentence and
	// a 409 quietly becomes a 500. domain.uniqueViolationTargets carries the same argument at length
	// about why identity is read structurally rather than from prose.
	switch {
	case errors.Is(err, scimBusiness.ErrScimUserNotFound):
		writeSCIMError(w, r, http.StatusNotFound, "", "no such user")
	case errors.Is(err, scimBusiness.ErrScimUserExists):
		// "uniqueness" is the scimType a directory recognises as "this account already exists", which is
		// what stops it retrying the create in a loop.
		writeSCIMError(w, r, http.StatusConflict, "uniqueness", scimBusiness.ErrScimUserExists.Error())
	case errors.Is(err, scimBusiness.ErrScimUserNameInvalid):
		writeSCIMError(w, r, http.StatusBadRequest, "invalidValue", scimBusiness.ErrScimUserNameInvalid.Error())
	case errors.Is(err, scimBusiness.ErrScimFilterUnsupported):
		writeSCIMError(w, r, http.StatusBadRequest, "invalidFilter", scimBusiness.ErrScimFilterUnsupported.Error())
	case errors.Is(err, scimBusiness.ErrScimPatchUnsupported):
		writeSCIMError(w, r, http.StatusBadRequest, "invalidValue", scimBusiness.ErrScimPatchUnsupported.Error())
	case helpers.IsSeatLimit(err):
		// 403, not 409 or 500: the request is understood and refused by policy,
		// and the message says how to lift it.
		writeSCIMError(w, r, http.StatusForbidden, "", err.Error())
	default:
		helpers.LogErrorWithContext(r.Context(),
			"controllers/scim credential=%s unexpected err: %+v",
			customMiddleware.ScimTokenIDFromContext(r.Context()), err)
		writeSCIMError(w, r, http.StatusInternalServerError, "", "the request could not be completed")
	}
}

// decodeSCIM reads a SCIM request body.
func decodeSCIM(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxSCIMBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeSCIMError(w, r, http.StatusBadRequest, "invalidSyntax", "the request body is not valid SCIM JSON")
		return false
	}
	return true
}

// ListScimUsers handles GET /scim/v2/Users, with optional ?filter=userName eq "..." and paging.
func ListScimUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// Ignoring the parse errors is intentional: a non-numeric startIndex leaves 0, and ListUsers
	// substitutes its documented default. Refusing the whole sync over an unparseable paging hint would
	// be a worse answer than serving the first page.
	startIndex, _ := strconv.Atoi(strings.TrimSpace(q.Get("startIndex")))
	count, _ := strconv.Atoi(strings.TrimSpace(q.Get("count")))

	list, err := scimBusiness.ListUsers(r.Context(), q.Get("filter"), startIndex, count, scimBaseURL(r))
	if err != nil {
		scimFailure(w, r, err)
		return
	}
	writeSCIM(w, r, http.StatusOK, list)
}

// GetScimUser handles GET /scim/v2/Users/{id}.
func GetScimUser(w http.ResponseWriter, r *http.Request) {
	res, err := scimBusiness.GetUser(r.Context(), chi.URLParam(r, "id"), scimBaseURL(r))
	if err != nil {
		scimFailure(w, r, err)
		return
	}
	writeSCIM(w, r, http.StatusOK, res)
}

// CreateScimUser handles POST /scim/v2/Users.
func CreateScimUser(w http.ResponseWriter, r *http.Request) {
	var in scimBusiness.ScimUserResource
	if !decodeSCIM(w, r, &in) {
		return
	}

	res, err := scimBusiness.CreateUser(r.Context(), in, scimBaseURL(r))
	if err != nil {
		scimFailure(w, r, err)
		return
	}

	helpers.LogInfoWithContext(r.Context(),
		"controllers/CreateScimUser provisioned user %s via credential %s",
		res.Id, customMiddleware.ScimTokenIDFromContext(r.Context()))

	// 201 with a Location header, as RFC 7644 §3.3 requires. Some providers read the header rather than
	// meta.location to learn the id they must use for every later call about this person.
	if res.Meta != nil && res.Meta.Location != "" {
		w.Header().Set("Location", res.Meta.Location)
	}
	writeSCIM(w, r, http.StatusCreated, res)
}

// PatchScimUser handles PATCH /scim/v2/Users/{id} — in practice, deactivation.
func PatchScimUser(w http.ResponseWriter, r *http.Request) {
	var req scimBusiness.ScimPatchRequest
	if !decodeSCIM(w, r, &req) {
		return
	}

	effect, err := scimBusiness.ResolvePatch(req)
	if err != nil {
		scimFailure(w, r, err)
		return
	}

	ctx := r.Context()
	id := chi.URLParam(r, "id")
	baseURL := scimBaseURL(r)

	if effect.DisplayName != nil {
		if _, err := scimBusiness.ReplaceUser(ctx, id,
			scimBusiness.ScimUserResource{DisplayName: *effect.DisplayName}, baseURL); err != nil {
			scimFailure(w, r, err)
			return
		}
	}

	if effect.SetActive != nil {
		res, activeErr := scimBusiness.SetActive(ctx, id, *effect.SetActive, baseURL)
		if activeErr != nil {
			scimFailure(w, r, activeErr)
			return
		}
		// Logged at INFO because it is the audit-relevant half of this whole surface: somebody's access
		// was granted or removed, and the credential that did it is named.
		helpers.LogInfoWithContext(ctx,
			"controllers/PatchScimUser set active=%t for user %s via credential %s",
			*effect.SetActive, id, customMiddleware.ScimTokenIDFromContext(ctx))
		writeSCIM(w, r, http.StatusOK, res)
		return
	}

	res, err := scimBusiness.GetUser(ctx, id, baseURL)
	if err != nil {
		scimFailure(w, r, err)
		return
	}
	writeSCIM(w, r, http.StatusOK, res)
}

// ReplaceScimUser handles PUT /scim/v2/Users/{id}.
func ReplaceScimUser(w http.ResponseWriter, r *http.Request) {
	var in scimBusiness.ScimUserResource
	if !decodeSCIM(w, r, &in) {
		return
	}

	res, err := scimBusiness.ReplaceUser(r.Context(), chi.URLParam(r, "id"), in, scimBaseURL(r))
	if err != nil {
		scimFailure(w, r, err)
		return
	}
	writeSCIM(w, r, http.StatusOK, res)
}

// DeleteScimUser handles DELETE /scim/v2/Users/{id}.
//
// Deactivates rather than destroys — see business/Scim.DeleteUser for why a hard delete is not available
// for an established user. 204 with no body, per RFC 7644 §3.6.
func DeleteScimUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := scimBusiness.DeleteUser(r.Context(), id); err != nil {
		scimFailure(w, r, err)
		return
	}
	helpers.LogInfoWithContext(r.Context(),
		"controllers/DeleteScimUser deactivated user %s via credential %s",
		id, customMiddleware.ScimTokenIDFromContext(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

// GetScimServiceProviderConfig handles GET /scim/v2/ServiceProviderConfig.
//
// Discovery, and it earns its place rather than being spec completionism: Okta and Azure AD both read
// this during connection setup, and a 404 is reported to the operator as a failed test with no
// explanation. Every capability below is declared as it is actually implemented — advertising `patch:
// false` when PATCH works would stop a directory from ever sending a deactivation, and advertising
// filter support that does not exist would make it send filters that are refused.
func GetScimServiceProviderConfig(w http.ResponseWriter, r *http.Request) {
	writeSCIM(w, r, http.StatusOK, map[string]any{
		"schemas":          []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"documentationUri": "https://onecamp.in/docs/scim",
		"patch":            map[string]any{"supported": true},
		"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
		"filter":           map[string]any{"supported": true, "maxResults": scimBusiness.MaxPageSize},
		"changePassword":   map[string]any{"supported": false},
		"sort":             map[string]any{"supported": false},
		"etag":             map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"type":        "oauthbearertoken",
			"name":        "OAuth Bearer Token",
			"description": "Authentication using a OneCamp SCIM provisioning credential as a bearer token.",
			"primary":     true,
		}},
		"meta": map[string]any{
			"resourceType": "ServiceProviderConfig",
			"location":     scimBaseURL(r) + "/ServiceProviderConfig",
		},
	})
}
