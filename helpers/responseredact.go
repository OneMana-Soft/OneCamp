package helpers

// redactedErrorText replaces an error value on its way into an HTTP response.
//
// Deliberately says where the detail went. A bare "error" tells a developer nothing; this points
// them at the server logs, which is where the real error already is — every controller logs it
// before responding.
const redactedErrorText = "redacted: see server logs"

// RedactErrorsInResponse returns data with any error value replaced by a fixed, safe string.
//
// WHY THIS EXISTS AT THE CHOKEPOINT. Roughly 790 controller response bodies were built as
//
//	helpers.Envolope{"msg": "...", "err": err}
//
// and a *pq.Error has exported fields, so encoding one hands the client the constraint name, the
// table and the column, plus Postgres' own source file, line and internal routine. I verified that
// by marshalling one rather than assuming it. Nothing consumes the field: the frontend reads only
// msg, mag and message, so those bodies were leaking schema detail to no purpose.
//
// Fixing it here rather than at 790 call sites is a deliberate choice, and not only for effort. A
// commit touching 790 handlers is not a commit anyone can review, and it would not stop the 791st
// from reintroducing it. Every response in the product goes through WriteJSON — 2,667 calls in
// controllers against 6 places that write a body directly.
//
// WHAT IT DOES NOT COVER, stated plainly because the earlier version of this comment claimed the
// class was closed "permanently, including for code not yet written" and that was wrong.
//
// It matches the error INTERFACE. A handler writing
//
//	helpers.Envolope{"msg": "...", "err": err.Error()}
//
// puts a STRING in the envelope, which reaches the client untouched. 109 controller sites across 14
// files do that in the err or error key today, and more do it in msg. For a *pq.Error that string is
// the driver's message, which still names the constraint — less than the full struct, and still more
// than a client needs.
//
// The matching is by VALUE TYPE, not by key name, which is worth stating because it cuts both ways:
// it means a handler cannot hide an error under an unusual key, and it means "error" is exactly as
// exposed as "err" when a string is used.
//
// It is not simply extended to strings, and the reason is not oversight. Some of those strings are
// written FOR the reader and are the whole point of the response: "the stored API key can no longer
// be decrypted (usually because AI_CONFIG_KEK changed); re-enter the key in admin AI settings" is
// what turns a dead end into one field to fix. Blanking every string under err would delete those. And deciding by message text —
// redacting anything containing "pq:" or "constraint" — fails in the direction that matters, since
// a wording or locale change makes it MISS and leak, which is the same argument that kept
// IsUniqueViolation off message matching.
//
// The real distinction is whether the SERVER AUTHORED the message for the reader, and a string
// cannot carry that. It has to be expressed at the source: a sentinel error the handler recognises
// and puts in msg (see business/AI.ErrProviderKeyUnreadable and AIMCP.ErrAuthSecretUnreadable),
// with everything else left to err and blanked here. Migrating those sites is that work, not a wider
// match in this function. helpers/testdata/err-string-baseline.txt is the work list, and
// TestErrorStringsInResponsesDoNotGrow stops it lengthening in the meantime.
//
// The key is KEPT and its value replaced, rather than the key being dropped, so response shape
// does not change for anything parsing it.
//
// A COPY is returned. Mutating the caller's map would be a surprising side effect on a value they
// may still hold, and envelopes are sometimes built once and reused.
//
// Only map-shaped envelopes are walked. That is the shape the leak takes, and reflecting over
// arbitrary structs to hunt for error-typed fields would be a large amount of machinery for a
// problem that does not exist — a controller returning a struct is returning a type it defined,
// with fields it chose.
func RedactErrorsInResponse(data interface{}) interface{} {
	switch typed := data.(type) {
	case Envolope:
		return Envolope(redactErrorsInMap(typed))
	case map[string]interface{}:
		return redactErrorsInMap(typed)
	default:
		return data
	}
}

// redactErrorsInMap copies m, replacing error values. Nested maps are walked so an envelope with a
// "data" sub-object is covered too.
func redactErrorsInMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for key, value := range m {
		switch inner := value.(type) {
		case error:
			// The error INTERFACE, not a concrete type: this has to catch *pq.Error, wrapped
			// errors, and anything else a handler happens to put here.
			out[key] = redactedErrorText
		case Envolope:
			out[key] = Envolope(redactErrorsInMap(inner))
		case map[string]interface{}:
			out[key] = redactErrorsInMap(inner)
		default:
			out[key] = value
		}
	}
	return out
}
