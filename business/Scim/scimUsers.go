package business

// SCIM 2.0 User provisioning.
//
// THE ONE THING THIS FILE MUST GET RIGHT: `active` is the whole product. Everything else SCIM does is
// bookkeeping, but `active: false` is offboarding, and offboarding is the event an operator most expects
// to be immediate and total.
//
// So deactivation goes through userBusiness.DeactivateUser and NOT through a direct write to
// users.deleted_at, even though the column is right there and one UPDATE would look like it worked.
// DeactivateUser writes the timestamp to Postgres AND to the Dgraph node, and business/Principal.Assess
// — the gate that stops a departed employee's api_token from continuing to authorise AI agent work —
// reads the DGRAPH copy. A Postgres-only write leaves Postgres saying the person is gone while every
// agent they authorised keeps running. That is the exact bypass business/Principal was written to close,
// and it would be reintroduced by the more obvious implementation.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	fcmBusiness "github.com/akashc777/OneCamp/business/UserFCMToken"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	scimModel "github.com/akashc777/OneCamp/models/postgres/Scim"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// SCIM schema URNs (RFC 7643/7644). Literals rather than a generated constant set, because these are
// wire identifiers an IdP matches exactly and a typo must be visible where it is used.
const (
	SchemaUser         = "urn:ietf:params:scim:schemas:core:2.0:User"
	SchemaListResponse = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	SchemaError        = "urn:ietf:params:scim:api:messages:2.0:Error"
)

// There is deliberately no SchemaPatchOp constant. The PatchOp URN would only be used to VALIDATE an
// incoming PATCH's schemas field, and that check has to be refused: providers vary on whether they send
// it at all, so requiring it would reject working deactivations, and an unused constant is a claim to
// enforce something that is not enforced.

// DefaultPageSize is the page an IdP gets when it does not ask for one.
const DefaultPageSize = 100

// MaxPageSize caps what an IdP may ask for, so `count=100000` cannot turn one request into a full table
// read held in memory.
const MaxPageSize = 500

var (
	// ErrScimUserNotFound → 404.
	ErrScimUserNotFound = errors.New("user not found")
	// ErrScimUserExists → 409 with scimType "uniqueness".
	ErrScimUserExists = errors.New("a user with that userName already exists")
	// ErrScimUserNameInvalid → 400 with scimType "invalidValue".
	ErrScimUserNameInvalid = errors.New(
		"userName must be the user's email address; map email to userName in your identity provider")
	// ErrScimUserNameNotASCII → 400 with scimType "invalidValue".
	ErrScimUserNameNotASCII = errors.New(
		"userName has characters outside ASCII; OneCamp matches only addresses written in plain ASCII to accounts")
	// ErrScimFilterUnsupported → 400 with scimType "invalidFilter".
	ErrScimFilterUnsupported = errors.New(`only filters of the form userName eq "value" are supported`)
	// ErrScimPatchUnsupported → 400 with scimType "invalidValue".
	ErrScimPatchUnsupported = errors.New("this PATCH changes an attribute OneCamp does not manage")
)

// ---------- resource shapes ----------

// ScimName is the SCIM complex `name` attribute. Every field omitempty: an IdP that manages only a
// display name should not receive a payload full of empty strings implying we hold those attributes.
type ScimName struct {
	Formatted  string `json:"formatted,omitempty"`
	GivenName  string `json:"givenName,omitempty"`
	FamilyName string `json:"familyName,omitempty"`
}

// ScimEmail is one entry of the SCIM `emails` multi-valued attribute.
type ScimEmail struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// ScimMeta is the SCIM common `meta` attribute.
type ScimMeta struct {
	ResourceType string `json:"resourceType"`
	Created      string `json:"created,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	Location     string `json:"location,omitempty"`
}

// ScimUserResource is a SCIM User as this service emits and accepts it.
type ScimUserResource struct {
	Schemas     []string    `json:"schemas"`
	Id          string      `json:"id,omitempty"`
	UserName    string      `json:"userName"`
	Name        *ScimName   `json:"name,omitempty"`
	DisplayName string      `json:"displayName,omitempty"`
	Emails      []ScimEmail `json:"emails,omitempty"`
	// Active is a POINTER on the way in so "absent" is distinguishable from "false". A PUT that omits
	// active must not be read as a request to deactivate somebody.
	Active *bool     `json:"active,omitempty"`
	Meta   *ScimMeta `json:"meta,omitempty"`
	// ExternalId is accepted and echoed back within a single request only. OneCamp does not store it,
	// so it is absent from reads; IdPs correlate on `id`, which is returned everywhere and stable.
	ExternalId string `json:"externalId,omitempty"`
}

// ScimListResponse is the SCIM envelope for a collection.
type ScimListResponse struct {
	Schemas []string `json:"schemas"`
	// TotalResults is the UNPAGED total, which is what an IdP uses to decide whether to fetch another
	// page. Returning the page length here makes a directory stop after the first page.
	TotalResults int                `json:"totalResults"`
	StartIndex   int                `json:"startIndex"`
	ItemsPerPage int                `json:"itemsPerPage"`
	Resources    []ScimUserResource `json:"Resources"`
}

// ScimError is the SCIM error envelope.
type ScimError struct {
	Schemas []string `json:"schemas"`
	Detail  string   `json:"detail"`
	// Status is a STRING in SCIM, not a number (RFC 7644 §3.12). An integer here is a well-known way to
	// make strict clients reject the body and report a parse failure instead of the actual error.
	Status   string `json:"status"`
	ScimType string `json:"scimType,omitempty"`
}

// ---------- mapping ----------

// ToResource renders a stored identity as a SCIM User.
//
// `active` is derived through helpers.IsSoftDeleted rather than a bare nil check. Checked before writing
// this: users.deleted_at only ever holds NULL or a real timestamp, so a nil test would in fact be correct
// TODAY — the three-encoding problem that helper exists for (NULL, Go zero, Dgraph epoch) belongs to the
// Dgraph copy, not to this column.
//
// It is still the right call, for a different reason than convenience. business/Principal.Assess decides
// whether a person may still authorize agent work, and it asks IsSoftDeleted about the Dgraph value. If
// this surface used its own predicate, SCIM could report somebody active whom the authorization gate
// treats as gone — and an operator reconciling the directory against actual access would get two
// contradictory answers with no way to tell which was enforced. One predicate, one answer.
func ToResource(u *scimModel.ScimUser, baseURL string) ScimUserResource {
	active := !helpers.IsSoftDeleted(u.DeletedAt)

	res := ScimUserResource{
		Schemas:  []string{SchemaUser},
		Id:       u.Id.String(),
		UserName: u.EmailID,
		Active:   &active,
		Emails: []ScimEmail{{
			Value:   u.EmailID,
			Type:    "work",
			Primary: true,
		}},
		Meta: &ScimMeta{
			ResourceType: "User",
			Created:      formatSCIMTime(u.CreatedAt),
			LastModified: formatSCIMTime(u.UpdatedAt),
			Location:     strings.TrimSuffix(baseURL, "/") + "/Users/" + u.Id.String(),
		},
	}

	display := ""
	if u.DisplayName != nil && strings.TrimSpace(*u.DisplayName) != "" {
		display = strings.TrimSpace(*u.DisplayName)
	} else if u.Username != nil && strings.TrimSpace(*u.Username) != "" {
		display = strings.TrimSpace(*u.Username)
	}
	if display != "" {
		res.DisplayName = display
		res.Name = &ScimName{Formatted: display}
	}
	return res
}

// formatSCIMTime renders a timestamp as the xsd:dateTime SCIM expects, or "" for a zero value.
func formatSCIMTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ---------- filtering ----------

// userNameFilter matches the one filter form this service implements.
//
// SCIM attribute names and operators are case-insensitive, hence (?i). The value is captured
// non-greedily up to the closing quote so a trailing `and ...` clause fails to match the anchor and is
// reported as unsupported rather than silently truncated into a lookup.
var userNameFilter = regexp.MustCompile(`(?i)^\s*userName\s+eq\s+"((?:[^"\\]|\\.)*)"\s*$`)

// ParseUserNameFilter extracts the target address from a SCIM filter.
//
// Returns ("", nil) when there is no filter, meaning "list everything".
//
// AN UNSUPPORTED FILTER IS AN ERROR, NOT A DEGRADATION, and this is the most important decision in the
// file after `active`. The tempting fallback is to ignore a filter we cannot parse and return the full
// list. An IdP asking `userName eq "someone@corp.com"` before deciding whether to create an account
// reads Resources[0] of whatever comes back — so ignoring the filter hands it an unrelated user and it
// concludes that person is the one it was looking for. It then updates, or deactivates, the wrong
// account. Failing loudly makes the operator fix the configuration; degrading quietly corrupts data.
func ParseUserNameFilter(filter string) (string, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return "", nil
	}
	m := userNameFilter.FindStringSubmatch(filter)
	if m == nil {
		return "", ErrScimFilterUnsupported
	}
	// Unescape the two sequences SCIM's JSON-string values can contain.
	value := strings.ReplaceAll(m[1], `\"`, `"`)
	value = strings.ReplaceAll(value, `\\`, `\`)
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ErrScimFilterUnsupported
	}
	return value, nil
}

// ---------- operations ----------

// ListUsers returns one SCIM page.
//
// startIndex is 1-BASED per RFC 7644 §3.4.2.4, unlike every internal pager in this codebase. Converted
// here rather than at the controller so the off-by-one lives in one place: an IdP that asks for
// startIndex=1 and receives the second user silently skips the first account in the directory.
func ListUsers(ctx context.Context, filter string, startIndex int, count int, baseURL string) (*ScimListResponse, error) {
	if startIndex < 1 {
		startIndex = 1
	}
	if count <= 0 {
		count = DefaultPageSize
	}
	if count > MaxPageSize {
		count = MaxPageSize
	}

	wanted, err := ParseUserNameFilter(filter)
	if err != nil {
		return nil, err
	}

	// A filtered lookup is a single-resource question. Answered with an exact read rather than a scan so
	// it stays one indexed lookup no matter how large the directory grows.
	if wanted != "" {
		u, lookupErr := scimModel.GetScimUserByEmail(ctx, wanted)
		if lookupErr != nil {
			return nil, lookupErr
		}
		out := &ScimListResponse{
			Schemas:      []string{SchemaListResponse},
			StartIndex:   1,
			Resources:    []ScimUserResource{},
			TotalResults: 0,
			ItemsPerPage: 0,
		}
		// NOT an error when there is no match. SCIM says a filter with no results is an empty
		// ListResponse, and a 404 here would make an IdP treat "this person needs an account" as a
		// failed request and retry it instead of creating one.
		if u != nil {
			out.Resources = append(out.Resources, ToResource(u, baseURL))
			out.TotalResults = 1
			out.ItemsPerPage = 1
		}
		return out, nil
	}

	users, total, err := scimModel.ListScimUsers(ctx, startIndex-1, count)
	if err != nil {
		return nil, err
	}

	resources := make([]ScimUserResource, 0, len(users))
	for _, u := range users {
		resources = append(resources, ToResource(u, baseURL))
	}
	return &ScimListResponse{
		Schemas:      []string{SchemaListResponse},
		TotalResults: total,
		StartIndex:   startIndex,
		ItemsPerPage: len(resources),
		Resources:    resources,
	}, nil
}

// GetUser returns one identity by SCIM id.
func GetUser(ctx context.Context, id string, baseURL string) (*ScimUserResource, error) {
	userID, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		// A malformed id cannot name a user, so this is the same answer as an id that names nobody.
		// Reporting it as a bad request instead would make an IdP retry a call that can never succeed.
		return nil, ErrScimUserNotFound
	}
	u, err := scimModel.GetScimUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrScimUserNotFound
	}
	res := ToResource(u, baseURL)
	return &res, nil
}

// CreateUser provisions an account from a SCIM User payload.
//
// Goes through userBusiness.JoinAsMember, the way every other way in makes a member, so a
// SCIM-provisioned account is identical to one made any other way: same Dgraph node, same OpenSearch
// document, same rollback if the graph write fails, the same default channels. Reimplementing the
// INSERT here would produce users the rest of the product cannot see, because every lookup in OneCamp
// reads Dgraph.
//
// JoinAsMember, not the creator beneath it, because of the person an import or a GitHub sync already
// knows: their address is on an external row (the author of what was imported), and email_id is
// unique. Creating an account refused that address with a 500 the directory could do nothing about;
// joining adopts the row, so the person gets their history and the directory its 201.
func CreateUser(ctx context.Context, in ScimUserResource, baseURL string) (*ScimUserResource, error) {
	email := helpers.NormalizeEmail(in.UserName)
	if email == "" {
		email = helpers.NormalizeEmail(primaryEmail(in))
	}
	if !looksLikeEmail(email) {
		return nil, ErrScimUserNameInvalid
	}
	// Refused before it is looked up: a lookup that folds case the Unicode
	// way would find someone else's account for it (helpers.NormalizeEmail).
	if !helpers.AddressIsASCII(email) {
		return nil, ErrScimUserNameNotASCII
	}

	// Existence is checked BEFORE inserting so the answer is 409 rather than a constraint violation, and
	// so a SOFT-DELETED user is reported as a conflict too. That is the correct SCIM answer and it is
	// also the only workable one: the row keeps the address, so the insert could never succeed. The IdP's
	// route back for a returning employee is to find them — list/get deliberately return deactivated
	// users — and PATCH active:true.
	existing, err := scimModel.GetScimUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrScimUserExists
	}

	// An account staged before the person starts (active:false) isn't
	// welcomed yet: SetActive does that, or their first sign-in.
	join := userBusiness.JoinAsMember
	if in.Active != nil && !*in.Active {
		join = userBusiness.JoinAsStagedMember
	}
	joined, err := join(
		ctx,
		email,
		deriveUserName(in, email),
		// No local password, ever. A SCIM-provisioned account authenticates at the identity provider
		// that provisioned it.
		nil,
		userModels.AuthMethodSCIM,
		// isSSOManaged: the set/change-password endpoints refuse these users, so an account created by
		// the directory cannot later be given a local password that bypasses the directory — including
		// after the directory has deactivated it.
		true,
	)
	if userDomain.IsUniqueViolationOnEmail(err) {
		// Another create for the same person won the race: the directory gets the same answer the
		// check above gives, not a 500 for a person who now exists.
		return nil, ErrScimUserExists
	}
	if err != nil {
		return nil, err
	}
	userID := joined.UserID

	// An IdP may send active:false on create, to stage an account before a start date. Honoured rather
	// than ignored: silently creating an active account for someone who has not started is exactly the
	// kind of quiet disagreement with the directory that makes an access review fail.
	if in.Active != nil && !*in.Active {
		if deactivateErr := deactivate(ctx, userID); deactivateErr != nil {
			return nil, deactivateErr
		}
	}

	created, err := scimModel.GetScimUserByID(ctx, userID)
	if err != nil || created == nil {
		// The account exists; only the read-back failed. Rendered from what we already know rather than
		// reported as a failure, because an error here would make the IdP retry a create that now
		// conflicts, and the operator would see a duplicate-user error for an account that is fine.
		res := ScimUserResource{
			Schemas:  []string{SchemaUser},
			Id:       userID.String(),
			UserName: email,
			Active:   in.Active,
			Meta: &ScimMeta{
				ResourceType: "User",
				Location:     strings.TrimSuffix(baseURL, "/") + "/Users/" + userID.String(),
			},
		}
		if res.Active == nil {
			live := true
			res.Active = &live
		}
		return &res, nil
	}
	res := ToResource(created, baseURL)
	res.ExternalId = in.ExternalId
	return &res, nil
}

// SetActive activates or deactivates an identity. Idempotent.
func SetActive(ctx context.Context, id string, active bool, baseURL string) (*ScimUserResource, error) {
	userID, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return nil, ErrScimUserNotFound
	}
	u, err := scimModel.GetScimUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrScimUserNotFound
	}

	currentlyActive := !helpers.IsSoftDeleted(u.DeletedAt)
	// Already in the requested state: return success and change nothing. Re-running DeactivateUser would
	// move deleted_at to now, rewriting WHEN someone was offboarded — and directories re-send the
	// current state routinely, so that timestamp would drift away from the real event it records.
	if currentlyActive == active {
		res := ToResource(u, baseURL)
		return &res, nil
	}

	if active {
		if err := userBusiness.ActivateUser(ctx, userID); err != nil {
			return nil, err
		}
		// Someone the directory staged (created inactive) and who has never
		// signed in starts now: the default channels, as joining gives
		// everyone else. Someone returning has been welcomed before.
		if userDomain.FirstSignInOfProvisioned(ctx, userID) {
			userBusiness.WelcomeNewMember(ctx, userID)
		}
	} else if err := deactivate(ctx, userID); err != nil {
		return nil, err
	}

	updated, err := scimModel.GetScimUserByID(ctx, userID)
	if err != nil || updated == nil {
		return nil, ErrScimUserNotFound
	}
	res := ToResource(updated, baseURL)
	return &res, nil
}

// ReplaceUser applies a SCIM PUT: the payload is the desired full state of the resource.
//
// userName is NOT changeable here. It maps to users.email_id, which is the account's identity across 55
// referencing tables and every audit record already written; a directory renaming an address must not
// silently re-point an existing account. A PUT carrying a different userName is refused rather than
// half-applied.
func ReplaceUser(ctx context.Context, id string, in ScimUserResource, baseURL string) (*ScimUserResource, error) {
	userID, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return nil, ErrScimUserNotFound
	}
	u, err := scimModel.GetScimUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrScimUserNotFound
	}

	// Byte for byte after NormalizeEmail: EqualFold folds the Kelvin sign
	// into "k", so another address could pass for this one.
	if sent := helpers.NormalizeEmail(in.UserName); sent != "" && sent != helpers.NormalizeEmail(u.EmailID) {
		return nil, ErrScimUserNameInvalid
	}

	if display := deriveDisplayName(in); display != "" {
		name := display
		if err := scimModel.UpdateScimUserNames(ctx, userID, nil, &name); err != nil {
			return nil, err
		}
	}

	// Active last, so a failure to write a display name cannot leave someone deactivated by a request
	// that was really about their name.
	if in.Active != nil {
		return SetActive(ctx, id, *in.Active, baseURL)
	}

	updated, err := scimModel.GetScimUserByID(ctx, userID)
	if err != nil || updated == nil {
		return nil, ErrScimUserNotFound
	}
	res := ToResource(updated, baseURL)
	return &res, nil
}

// DeleteUser handles SCIM DELETE.
//
// A SOFT delete, mapped to the same deactivation path as active:false. Two reasons, and the first is
// sufficient on its own: users(id) is referenced by 55 tables with a mix of CASCADE, SET NULL and
// RESTRICT, so removing an established user either fails or takes their authored history with it —
// models/postgres/User.HardDeleteUser documents itself as compensation for a just-inserted row and
// nothing else. Second, SCIM does not require the row to be destroyed; it requires the resource to stop
// being usable, and deactivation achieves that while a person's messages and documents keep an author.
func DeleteUser(ctx context.Context, id string) error {
	userID, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return ErrScimUserNotFound
	}
	u, err := scimModel.GetScimUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return ErrScimUserNotFound
	}
	if helpers.IsSoftDeleted(u.DeletedAt) {
		// Already gone. DELETE is idempotent, and a directory retrying a delete it already made must not
		// see a failure it will keep retrying.
		return nil
	}
	return deactivate(ctx, userID)
}

// deactivate is the single deactivation path for every SCIM route that removes access.
//
// Wrapped rather than called directly in four places so the push-token cleanup cannot be forgotten in
// one of them. That cleanup lives in the admin HTTP handler rather than in DeactivateUser, so calling
// only the business function — the obvious thing to do — leaves a deprovisioned person still receiving
// mobile notifications from a workspace they can no longer open.
func deactivate(ctx context.Context, userID uuid.UUID) error {
	if err := userBusiness.DeactivateUser(ctx, userID); err != nil {
		return err
	}
	// Best-effort and asynchronous, matching controllers/User.DeactivateUser. Access is already revoked
	// by the call above; this stops the notifications, and failing the SCIM request because a push-token
	// row would not delete would tell the directory that offboarding did not happen when it did.
	go fcmBusiness.DeleteByUserId(userID.String())
	return nil
}

// ---------- payload helpers ----------

// looksLikeEmail is a deliberately shallow check.
//
// It is not validating the address — the directory is authoritative about who its people are, and a
// regex here would eventually refuse somebody's real address. It exists to catch a MISCONFIGURATION: an
// IdP sending sAMAccountName as userName, where accepting it would write "jsmith" into a column the rest
// of the product reads as an email address and mails to.
func looksLikeEmail(s string) bool {
	at := strings.Index(s, "@")
	return at > 0 && at < len(s)-1 && !strings.ContainsAny(s, " \t\r\n")
}

// primaryEmail returns the primary address from a SCIM payload, or the first one given.
func primaryEmail(in ScimUserResource) string {
	for _, e := range in.Emails {
		if e.Primary && strings.TrimSpace(e.Value) != "" {
			return strings.TrimSpace(e.Value)
		}
	}
	for _, e := range in.Emails {
		if strings.TrimSpace(e.Value) != "" {
			return strings.TrimSpace(e.Value)
		}
	}
	return ""
}

// deriveDisplayName picks the best human name available in a SCIM payload.
func deriveDisplayName(in ScimUserResource) string {
	if s := strings.TrimSpace(in.DisplayName); s != "" {
		return s
	}
	if in.Name != nil {
		if s := strings.TrimSpace(in.Name.Formatted); s != "" {
			return s
		}
		joined := strings.TrimSpace(strings.TrimSpace(in.Name.GivenName) + " " + strings.TrimSpace(in.Name.FamilyName))
		if joined != "" {
			return joined
		}
	}
	return ""
}

// deriveUserName produces the in-product name for a new account.
//
// Falls back to the local part of the address, because a person with no name at all renders as a blank
// avatar and an empty mention everywhere in the product. It is a display name, which need not be
// unique: the handle CreateUserWithMethod derives from it is, so a second "jsmith" is @jsmith-2 rather
// than refused.
func deriveUserName(in ScimUserResource, email string) string {
	if s := deriveDisplayName(in); s != "" {
		return s
	}
	if at := strings.Index(email, "@"); at > 0 {
		return email[:at]
	}
	return email
}
