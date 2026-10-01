package helpers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lib/pq"
)

func driverError() *pq.Error {
	return &pq.Error{
		Severity:   "ERROR",
		Code:       "23505",
		Message:    `duplicate key value violates unique constraint "channels_ch_name_key"`,
		Detail:     `Key (ch_name)=(general) already exists.`,
		Table:      "channels",
		Column:     "ch_name",
		Constraint: "channels_ch_name_key",
		File:       "nbtinsert.c",
		Line:       "666",
		Routine:    "_bt_check_unique",
	}
}

// The disclosure this closes, asserted end to end through the real writer rather than against the
// redactor in isolation — the point is what goes on the wire.
func TestWriteJSONDoesNotLeakDriverErrorDetail(t *testing.T) {
	rec := httptest.NewRecorder()

	WriteJSON(rec, 500, Envolope{"msg": "Failed to create channel", "err": driverError()})

	body := rec.Body.String()
	for _, leaked := range []string{
		"channels_ch_name_key", // the constraint
		"channels",             // the table
		"ch_name",              // the column
		"nbtinsert.c",          // Postgres' source file
		"_bt_check_unique",     // its internal routine
		"23505",                // the SQLSTATE
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("response body leaked %q:\n%s", leaked, body)
		}
	}

	// The message the handler wrote must survive; only the error is redacted.
	if !strings.Contains(body, "Failed to create channel") {
		t.Errorf("the handler's own message was lost:\n%s", body)
	}
}

// The key is kept rather than dropped, so anything parsing the shape still finds it.
func TestRedactionKeepsTheKeyAndReplacesTheValue(t *testing.T) {
	out := RedactErrorsInResponse(Envolope{"err": errors.New("boom")})

	env, ok := out.(Envolope)
	if !ok {
		t.Fatalf("type changed: got %T, want Envolope", out)
	}
	v, present := env["err"]
	if !present {
		t.Fatal("the err key was dropped; response shape changed for anything reading it")
	}
	if v != redactedErrorText {
		t.Errorf("err = %v, want %q", v, redactedErrorText)
	}
}

// A wrapped error is still an error. Handlers wrap freely, and a concrete-type check would miss
// these — which is the whole reason the switch matches the interface.
func TestRedactionCatchesWrappedErrors(t *testing.T) {
	wrapped := fmt.Errorf("business/CreateChannel: %w", driverError())

	out := RedactErrorsInResponse(Envolope{"err": wrapped}).(Envolope)
	if out["err"] != redactedErrorText {
		t.Errorf("a wrapped driver error survived redaction: %v", out["err"])
	}
}

// Everything that is not an error must pass through untouched, or this quietly corrupts every
// successful response in the product.
func TestRedactionLeavesNonErrorValuesAlone(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}

	in := Envolope{
		"msg":    "Created channel!",
		"count":  42,
		"ok":     true,
		"data":   payload{Name: "general"},
		"list":   []string{"a", "b"},
		"nilval": nil,
	}
	out := RedactErrorsInResponse(in).(Envolope)

	if out["msg"] != "Created channel!" || out["count"] != 42 || out["ok"] != true {
		t.Errorf("scalar values were altered: %+v", out)
	}
	if got, ok := out["data"].(payload); !ok || got.Name != "general" {
		t.Errorf("a struct payload was altered: %+v", out["data"])
	}
	if len(out["list"].([]string)) != 2 {
		t.Errorf("a slice was altered: %+v", out["list"])
	}
	if out["nilval"] != nil {
		t.Errorf("a nil value was altered: %+v", out["nilval"])
	}
}

// The caller's map must not be mutated. Envelopes are sometimes built once and reused, and a
// hidden side effect on a shared map is both surprising and a data race waiting to happen.
func TestRedactionDoesNotMutateTheCallersMap(t *testing.T) {
	original := errors.New("boom")
	in := Envolope{"err": original}

	_ = RedactErrorsInResponse(in)

	if in["err"] != original {
		t.Error("the caller's envelope was mutated; a reused envelope would lose its error")
	}
}

// A nested object gets walked too — an envelope carrying {"data": {"err": err}} leaks just as
// readily as a top-level one.
func TestRedactionWalksNestedMaps(t *testing.T) {
	out := RedactErrorsInResponse(Envolope{
		"data": map[string]interface{}{"err": driverError(), "name": "general"},
	}).(Envolope)

	nested := out["data"].(map[string]interface{})
	if nested["err"] != redactedErrorText {
		t.Errorf("a nested error survived: %v", nested["err"])
	}
	if nested["name"] != "general" {
		t.Errorf("a nested non-error value was altered: %v", nested["name"])
	}
}

// Content-Type was misspelled "applicaiton/json" on every response this product has returned.
func TestWriteJSONSendsAValidContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, 200, Envolope{"msg": "ok"})

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
}

// And the body must still be valid JSON after all of the above.
func TestWriteJSONStillEmitsValidJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, 409, Envolope{"msg": "conflict", "err": driverError(), "status": "failed"})

	var decoded map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	if decoded["msg"] != "conflict" || decoded["status"] != "failed" {
		t.Errorf("fields lost in round trip: %+v", decoded)
	}
	if decoded["err"] != redactedErrorText {
		t.Errorf("err = %v, want the redaction marker", decoded["err"])
	}
}
