package business

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	"github.com/google/uuid"
)

// Every field on the admin ProviderView must actually be populated by toProviderView.
//
// WHY THIS IS A REFLECTION TEST AND NOT THREE ASSERTIONS. The bug it guards is an omission, and an
// omission is invisible in the one place you would look. models/postgres/AI set KeyUnreadable and
// tagged it `json:"key_unreadable"`, so the flag looked shipped from the model's side. But the admin
// endpoint does not serialise the model — it serialises adapter.ProviderView, and the mapper simply
// did not copy the field across. Nothing failed. No build error, no vet warning, no test: the
// frontend received a payload with the field absent, read undefined, and rendered a provider whose
// stored key could not be decrypted as if it were perfectly healthy. Every request through it
// failed with an authentication error, which is what beta reported.
//
// A hand-written assertion per field has the same hole as the mapper: whoever forgets to copy the
// field forgets to assert it. Enumerating the destination struct at runtime is the only version that
// covers the field NOBODY HAS ADDED YET, which is the one that will be forgotten.
//
// HOW IT WORKS. Fill every field of the source with a non-zero value, map, then require every field
// of the destination to be non-zero. It deliberately does not compare values: ID and UpdatedAt are
// converted on the way through (uuid to string, time to a formatted string), so a value comparison
// would need the conversions restated here, which is just the mapper written twice. Non-zero is
// enough to prove the field was reached, and reaching it is exactly what was missing.
func TestProviderViewIsFullyPopulated(t *testing.T) {
	provider := &aiModels.AIProvider{}
	fillNonZero(t, reflect.ValueOf(provider).Elem())

	view := reflect.ValueOf(toProviderView(provider))
	viewType := view.Type()

	for i := 0; i < view.NumField(); i++ {
		field := viewType.Field(i)
		if view.Field(i).IsZero() {
			t.Errorf("toProviderView leaves %s.%s at its zero value even though every source field "+
				"was set. Either map it from aiModels.AIProvider, or if it genuinely has no source, "+
				"say so here — a field that is always zero is a field the frontend reads as absent.",
				viewType.Name(), field.Name)
		}
	}
}

// The view must not carry the decrypted key.
//
// Pinned next to the completeness check because the two pull in opposite directions: that one says
// "copy everything across", and someone acting on it literally would copy APIKey too. AIProvider
// holds the PLAINTEXT key — it is tagged `json:"-"` there for exactly this reason — and ProviderView
// is what the admin page receives over the wire.
//
// Checked by field name rather than by tag, because omitting the field is the only safe answer. A
// `json:"-"` tag on the view would keep it out of the response body while still copying the
// plaintext into a struct that gets passed around and logged.
func TestProviderViewCarriesNoSecret(t *testing.T) {
	viewType := reflect.TypeOf(adapter.ProviderView{})

	for _, forbidden := range []string{"APIKey", "ApiKey", "Key", "Secret", "Token", "Password"} {
		if _, found := viewType.FieldByName(forbidden); found {
			t.Errorf("adapter.ProviderView has a %s field. This struct is serialised to the admin "+
				"page; the provider's decrypted key must never leave models/postgres/AI. Send a "+
				"boolean about the key instead, as HasAPIKey and KeyUnreadable do.", forbidden)
		}
	}
}

// The wire contract the admin page is written against.
//
// The mapping test above proves the Go field is populated; this proves it is SPELLED the way the
// frontend reads it. Those are different failures with the same symptom — a field the UI sees as
// absent — and a struct tag typo is invisible to the compiler.
//
// onecamp-fe declares `key_unreadable?: boolean` on ProviderView and renders a persistent "key
// unreadable" badge from it. That name is the contract; the assertions below are what makes renaming
// it here a test failure rather than a silently dead badge.
func TestProviderViewWireContract(t *testing.T) {
	t.Run("an unreadable key is reported", func(t *testing.T) {
		raw, err := json.Marshal(adapter.ProviderView{KeyUnreadable: true})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(raw), `"key_unreadable":true`) {
			t.Errorf("the admin page reads key_unreadable; got %s", raw)
		}
	})

	t.Run("a healthy provider omits it", func(t *testing.T) {
		// omitempty, so the frontend's optional boolean is undefined rather than false. Both are
		// falsy in TypeScript, so this is about payload noise rather than correctness — but it is
		// asserted because dropping omitempty would also change has_api_key's neighbours, and the
		// test then says which choice was deliberate.
		raw, err := json.Marshal(adapter.ProviderView{KeyUnreadable: false})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(raw), "key_unreadable") {
			t.Errorf("expected key_unreadable to be omitted when false; got %s", raw)
		}
	})

	t.Run("no key material is serialised", func(t *testing.T) {
		// Belt and braces with TestProviderViewCarriesNoSecret: that one checks the struct has no
		// such field, this one checks the encoded form, so a future embedded struct cannot smuggle
		// one in.
		view := adapter.ProviderView{}
		fillNonZero(t, reflect.ValueOf(&view).Elem())
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}

		// Decoded and checked KEY BY KEY, not as a substring of the body. Substring matching gets
		// this wrong in both directions: "api_key" is inside the perfectly fine "has_api_key", and
		// a field named "auth_token" would slip past a search for "token" spelled as a whole key.
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		// Booleans ABOUT the key are the whole point of this struct; the key itself is what must
		// not appear. Listed explicitly so adding a third one is a deliberate edit here.
		permitted := map[string]bool{"has_api_key": true, "key_unreadable": true}
		for key := range body {
			if permitted[key] {
				continue
			}
			lower := strings.ToLower(key)
			for _, word := range []string{"key", "secret", "token", "password", "credential"} {
				if strings.Contains(lower, word) {
					t.Errorf("encoded ProviderView has a %q field. The provider's decrypted key "+
						"must never leave models/postgres/AI; if this is a boolean ABOUT the key "+
						"rather than the key, add it to the permitted list above. Body: %s", key, raw)
				}
			}
		}
	})
}

// fillNonZero sets every field of a struct to a non-zero value.
//
// It FAILS on a type it does not know rather than skipping it. A silent skip would mean a new field
// on AIProvider is left zero here, so the completeness check above would pass whether or not the
// mapper copies it — the check would stop working at precisely the moment it was needed.
func fillNonZero(t *testing.T, structValue reflect.Value) {
	t.Helper()

	uuidType := reflect.TypeOf(uuid.UUID{})
	timeType := reflect.TypeOf(time.Time{})

	for i := 0; i < structValue.NumField(); i++ {
		field := structValue.Field(i)
		name := structValue.Type().Field(i).Name
		if !field.CanSet() {
			t.Fatalf("%s is unexported; this test cannot fill it, so completeness cannot be checked", name)
		}

		switch {
		case field.Type() == uuidType:
			field.Set(reflect.ValueOf(uuid.New()))
		case field.Type() == timeType:
			field.Set(reflect.ValueOf(time.Now()))
		default:
			switch field.Kind() {
			case reflect.String:
				field.SetString("non-zero")
			case reflect.Bool:
				field.SetBool(true)
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				field.SetInt(1)
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				field.SetUint(1)
			case reflect.Float32, reflect.Float64:
				field.SetFloat(1)
			default:
				t.Fatalf("field %s has kind %s, which fillNonZero does not handle. Add it — "+
					"leaving it zero would quietly disable TestProviderViewIsFullyPopulated for "+
					"that field.", name, field.Kind())
			}
		}
	}
}
