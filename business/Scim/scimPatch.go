package business

// SCIM PATCH, which is where real identity providers disagree with each other and with the RFC.
//
// RFC 7644 §3.5.2 defines one shape. Deployed providers send at least four, and a parser that handles
// only the documented one fails on Azure AD — the single most common enterprise directory — while
// looking perfectly correct next to the specification.
//
// The variants that must all mean "deactivate this person":
//
//	{"op":"replace","path":"active","value":false}            RFC form, Okta
//	{"op":"replace","path":"active","value":"False"}          Azure AD: STRING, capitalised
//	{"op":"replace","value":{"active":false}}                 Azure AD: no path, object value
//	{"op":"Replace","path":"active","value":false}            capitalised op; §3.5.2 says case-insensitive
//	{"op":"replace","path":"urn:...:User:active","value":...} fully-qualified attribute path
//
// Getting this wrong has one direction of failure and it is the bad one: an unparsed deactivation is
// answered 200 OK while the account stays live. The directory believes the person is offboarded, the
// access review says they are offboarded, and they are not. So an operation this code does not
// understand is REFUSED rather than skipped — a 400 an operator can see beats a success that is a lie.

import (
	"encoding/json"
	"strings"
)

// ScimPatchOperation is one entry of a SCIM PatchOp.
//
// Value is json.RawMessage because its type varies by provider — bool, string, or object — and decoding
// into `any` would commit to a shape before knowing which one arrived.
type ScimPatchOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// ScimPatchRequest is the SCIM PatchOp envelope.
type ScimPatchRequest struct {
	Schemas    []string             `json:"schemas,omitempty"`
	Operations []ScimPatchOperation `json:"Operations"`
}

// ScimPatchEffect is what a PatchOp resolves to, in terms this service can act on.
type ScimPatchEffect struct {
	// SetActive is nil when no operation addressed `active`.
	SetActive *bool
	// DisplayName is nil when no operation addressed a name.
	DisplayName *string
}

// ResolvePatch reduces a PatchOp to its effect, or refuses it.
//
// Operations are applied in order, so a later one wins — that is what "sequentially" in §3.5.2 means, and
// it matters for a payload that sets a name and then deactivates.
func ResolvePatch(req ScimPatchRequest) (ScimPatchEffect, error) {
	var effect ScimPatchEffect
	if len(req.Operations) == 0 {
		return effect, ErrScimPatchUnsupported
	}

	for _, op := range req.Operations {
		verb := strings.ToLower(strings.TrimSpace(op.Op))
		// add and replace are equivalent for the single-valued attributes handled here: both mean "this
		// is the value now". remove is refused below, since removing `active` has no meaning — there is
		// no unset state for it — and removing a name is not something a directory should do to a person
		// who chose it in OneCamp.
		if verb != "replace" && verb != "add" {
			return effect, ErrScimPatchUnsupported
		}

		attr := normalisePatchPath(op.Path)

		// No path: the value is an object of attribute/value pairs. Azure AD's default shape.
		if attr == "" {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(op.Value, &fields); err != nil {
				return effect, ErrScimPatchUnsupported
			}
			for rawKey, rawVal := range fields {
				if err := applyPatchField(&effect, normalisePatchPath(rawKey), rawVal); err != nil {
					return effect, err
				}
			}
			continue
		}

		if err := applyPatchField(&effect, attr, op.Value); err != nil {
			return effect, err
		}
	}

	// Nothing recognised. Refused rather than answered 200, because the caller believes it changed
	// something.
	if effect.SetActive == nil && effect.DisplayName == nil {
		return effect, ErrScimPatchUnsupported
	}
	return effect, nil
}

// applyPatchField records one attribute assignment onto the accumulating effect.
func applyPatchField(effect *ScimPatchEffect, attr string, raw json.RawMessage) error {
	switch attr {
	case "active":
		v, ok := parseSCIMBool(raw)
		if !ok {
			return ErrScimPatchUnsupported
		}
		effect.SetActive = &v
		return nil

	case "displayname", "name.formatted":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ErrScimPatchUnsupported
		}
		s = strings.TrimSpace(s)
		if s == "" {
			// An empty name would leave a blank avatar and an empty mention everywhere in the product.
			// Ignored rather than written; not an error, because a provider clearing an attribute it does
			// not really manage should not fail an otherwise valid offboarding in the same payload.
			return nil
		}
		effect.DisplayName = &s
		return nil

	case "username", "externalid", "name", "name.givenname", "name.familyname", "emails", "emails[type eq \"work\"].value":
		// Accepted and ignored, deliberately, and this list is the reason ResolvePatch is not simply
		// permissive. These are attributes a directory legitimately sends and OneCamp either cannot
		// change (userName is the account's identity across 55 referencing tables) or does not store.
		// Ignoring them by NAME means an attribute nobody has considered still reaches the refusal
		// below — which is what stops a future provider's `active` spelling from being silently dropped.
		return nil

	default:
		return ErrScimPatchUnsupported
	}
}

// normalisePatchPath reduces a SCIM attribute path to a comparable key.
//
// Strips the schema URN prefix that fully-qualified paths carry, so `urn:ietf:params:scim:schemas:core:
// 2.0:User:active` and `active` are one case rather than two.
func normalisePatchPath(path string) string {
	p := strings.ToLower(strings.TrimSpace(path))
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "urn:") {
		// The attribute is whatever follows the final colon; URNs contain colons, so this cannot split on
		// the first one.
		if i := strings.LastIndex(p, ":"); i >= 0 && i < len(p)-1 {
			p = p[i+1:]
		}
	}
	return p
}

// parseSCIMBool reads a boolean that may have arrived as a JSON bool or as a quoted string.
//
// The string case is not defensive programming for its own sake: Azure AD sends "True" and "False", and
// a strict bool decode rejects the exact payload that offboards somebody.
func parseSCIMBool(raw json.RawMessage) (bool, bool) {
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}
