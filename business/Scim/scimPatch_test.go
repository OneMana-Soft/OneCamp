package business

import (
	"encoding/json"
	"errors"
	"testing"
)

// The PATCH resolver, tested against the shapes real directories actually send.
//
// This is the highest-value test in the SCIM surface because its failure mode is silent and severe: an
// operation the parser does not understand, if skipped rather than refused, is answered 200 OK while the
// account stays live. The directory records the person as offboarded, an access review reads the
// directory, and the account still works. Nothing in the system reports a problem.
//
// So each provider variant below is a wire format observed in the wild, not a hypothetical, and the
// refusal cases matter as much as the successes.

func mustPatch(t *testing.T, body string) ScimPatchRequest {
	t.Helper()
	var req ScimPatchRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("test fixture is not valid JSON: %v", err)
	}
	return req
}

func TestResolvePatchDeactivatesForEveryProviderShape(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			// RFC 7644 §3.5.2, and what Okta sends.
			name: "rfc form, real boolean",
			body: `{"Operations":[{"op":"replace","path":"active","value":false}]}`,
		},
		{
			// Azure AD sends the boolean as a CAPITALISED STRING. A strict bool decode rejects this, which
			// would mean the single most common enterprise directory cannot offboard anyone.
			name: "azure ad, string boolean",
			body: `{"Operations":[{"op":"replace","path":"active","value":"False"}]}`,
		},
		{
			name: "lowercase string boolean",
			body: `{"Operations":[{"op":"replace","path":"active","value":"false"}]}`,
		},
		{
			// Azure AD's other shape: no path, value is an object of attributes.
			name: "no path, object value",
			body: `{"Operations":[{"op":"replace","value":{"active":false}}]}`,
		},
		{
			// §3.5.2 says op is case-insensitive; some providers capitalise it.
			name: "capitalised op",
			body: `{"Operations":[{"op":"Replace","path":"active","value":false}]}`,
		},
		{
			// A fully-qualified attribute path. The URN contains colons, so the attribute is what follows
			// the LAST one — splitting on the first would yield "ietf".
			name: "fully qualified path",
			body: `{"Operations":[{"op":"replace","path":"urn:ietf:params:scim:schemas:core:2.0:User:active","value":false}]}`,
		},
		{
			// add and replace both mean "this is the value now" for a single-valued attribute.
			name: "add rather than replace",
			body: `{"Operations":[{"op":"add","path":"active","value":false}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			effect, err := ResolvePatch(mustPatch(t, tc.body))
			if err != nil {
				t.Fatalf("refused a deactivation a real directory sends: %v", err)
			}
			if effect.SetActive == nil {
				t.Fatal("SetActive is nil, so this payload would be answered 200 OK with the account still live")
			}
			if *effect.SetActive {
				t.Fatalf("read a deactivation as an ACTIVATION, which is the worst possible misreading")
			}
		})
	}
}

func TestResolvePatchActivates(t *testing.T) {
	// The return path for a re-hire. It has to work, because a soft-deleted user keeps their unique
	// email address, so re-creating them is impossible and this is the only route back.
	for _, body := range []string{
		`{"Operations":[{"op":"replace","path":"active","value":true}]}`,
		`{"Operations":[{"op":"replace","path":"active","value":"True"}]}`,
		`{"Operations":[{"op":"replace","value":{"active":true}}]}`,
	} {
		effect, err := ResolvePatch(mustPatch(t, body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if effect.SetActive == nil || !*effect.SetActive {
			t.Fatalf("%s: did not resolve to an activation", body)
		}
	}
}

func TestResolvePatchRefusesRatherThanSilentlySucceeding(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			// The case this whole design exists for: an attribute nobody has considered. It must NOT be
			// skipped, because the caller believes it changed something.
			name: "unknown attribute",
			body: `{"Operations":[{"op":"replace","path":"enabled","value":false}]}`,
		},
		{
			// A future provider spelling `active` differently reaches here rather than being dropped.
			name: "unknown attribute in an object value",
			body: `{"Operations":[{"op":"replace","value":{"accountEnabled":false}}]}`,
		},
		{
			// remove has no meaning for active — there is no unset state — and should not be guessed at.
			name: "remove op",
			body: `{"Operations":[{"op":"remove","path":"active"}]}`,
		},
		{
			name: "no operations at all",
			body: `{"Operations":[]}`,
		},
		{
			// A value that is neither a boolean nor a boolean-ish string. Guessing would be worse than
			// refusing: "0" and "no" are not things this code has agreed to interpret.
			name: "non-boolean active value",
			body: `{"Operations":[{"op":"replace","path":"active","value":"maybe"}]}`,
		},
		{
			name: "active value is a number",
			body: `{"Operations":[{"op":"replace","path":"active","value":0}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolvePatch(mustPatch(t, tc.body))
			if !errors.Is(err, ErrScimPatchUnsupported) {
				t.Fatalf("expected a refusal, got err=%v — an unrefused unknown op is answered 200 with nothing changed", err)
			}
		})
	}
}

func TestResolvePatchIgnoresAttributesOneCampDoesNotManageWithoutFailingTheRequest(t *testing.T) {
	// A directory routinely sends a name or an externalId alongside the deactivation. Those must not
	// fail the payload, because the deactivation in the same request is the part that matters.
	effect, err := ResolvePatch(mustPatch(t,
		`{"Operations":[
			{"op":"replace","path":"externalId","value":"okta-123"},
			{"op":"replace","path":"userName","value":"someone.else@corp.com"},
			{"op":"replace","path":"active","value":false}
		]}`))
	if err != nil {
		t.Fatalf("a payload carrying unmanaged attributes plus a deactivation was refused: %v", err)
	}
	if effect.SetActive == nil || *effect.SetActive {
		t.Fatal("the deactivation was lost among the ignored attributes")
	}
}

func TestResolvePatchAppliesOperationsInOrder(t *testing.T) {
	// §3.5.2 says operations apply sequentially, so the last write to an attribute wins. A resolver that
	// took the first would deactivate somebody a directory had just decided to keep.
	effect, err := ResolvePatch(mustPatch(t,
		`{"Operations":[
			{"op":"replace","path":"active","value":false},
			{"op":"replace","path":"active","value":true}
		]}`))
	if err != nil {
		t.Fatal(err)
	}
	if effect.SetActive == nil || !*effect.SetActive {
		t.Fatal("the later operation did not win")
	}
}

func TestResolvePatchReadsDisplayName(t *testing.T) {
	effect, err := ResolvePatch(mustPatch(t,
		`{"Operations":[{"op":"replace","path":"displayName","value":"Ada Lovelace"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if effect.DisplayName == nil || *effect.DisplayName != "Ada Lovelace" {
		t.Fatalf("displayName not resolved: %+v", effect.DisplayName)
	}
	if effect.SetActive != nil {
		t.Fatal("a name change must not imply anything about active")
	}
}

func TestResolvePatchRefusesAnEmptyNameOnlyPayload(t *testing.T) {
	// Blanking a name is ignored, so a payload that ONLY blanks a name resolves to no effect at all —
	// and no effect must be a refusal, not a 200. Otherwise the directory believes it renamed somebody.
	_, err := ResolvePatch(mustPatch(t,
		`{"Operations":[{"op":"replace","path":"displayName","value":"   "}]}`))
	if !errors.Is(err, ErrScimPatchUnsupported) {
		t.Fatalf("expected a refusal for a payload with no net effect, got %v", err)
	}
}

func TestResolvePatchKeepsAnEmptyNameFromWipingAChosenOne(t *testing.T) {
	// The same blanking, this time alongside a real deactivation: the request succeeds, and the empty
	// name is dropped rather than written. A user who set their own display name in OneCamp should not
	// have it cleared by a directory that does not really manage it.
	effect, err := ResolvePatch(mustPatch(t,
		`{"Operations":[
			{"op":"replace","path":"displayName","value":""},
			{"op":"replace","path":"active","value":false}
		]}`))
	if err != nil {
		t.Fatal(err)
	}
	if effect.DisplayName != nil {
		t.Fatalf("an empty name was accepted as a value to write: %q", *effect.DisplayName)
	}
	if effect.SetActive == nil || *effect.SetActive {
		t.Fatal("the deactivation was lost")
	}
}
