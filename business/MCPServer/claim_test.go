package business

import (
	"os"
	"regexp"
	"strings"
	"testing"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// keyFuncFor builds the derivation function a bridged tool would get, so the tests
// exercise the real generated function rather than a stand-in.
func keyFuncFor(t *testing.T, tool string) func(map[string]any) (string, error) {
	t.Helper()
	def, ok := staticToolDef(tool)
	if !ok {
		t.Fatalf("%q is not in ai.ToolRegistry", tool)
	}
	fn := idempotencyKeyFor(def, ToolBehaviour{ReadOnly: false, Idempotent: false})
	if fn == nil {
		t.Fatalf("no key function generated for %q", tool)
	}
	return fn
}

// THE PROPERTY THE WHOLE MECHANISM RESTS ON: the same call must derive the same key
// every time. If it did not, a retry would look like a new write and duplicate it —
// which is the bug this exists to prevent.
func TestIdempotencyKeyIsStableForTheSameCall(t *testing.T) {
	fn := keyFuncFor(t, "send_message")
	args := map[string]any{"channel_uuid": "c-1", "text": "deploy finished"}

	first, err := fn(args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, aerr := fn(map[string]any{"text": "deploy finished", "channel_uuid": "c-1"})
		if aerr != nil {
			t.Fatalf("unexpected error: %v", aerr)
		}
		if again != first {
			t.Fatalf("key is not stable across calls or map orderings:\n  %q\n  %q", first, again)
		}
	}
}

// A different call must derive a different key, or two distinct writes would collapse
// into one and the second would silently never happen.
func TestIdempotencyKeyDistinguishesDifferentCalls(t *testing.T) {
	fn := keyFuncFor(t, "send_message")

	base, _ := fn(map[string]any{"channel_uuid": "c-1", "text": "hello"})
	for name, args := range map[string]map[string]any{
		"different text":    {"channel_uuid": "c-1", "text": "hello there"},
		"different channel": {"channel_uuid": "c-2", "text": "hello"},
		"empty text":        {"channel_uuid": "c-1", "text": ""},
		"missing text":      {"channel_uuid": "c-1"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := fn(args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == base {
				t.Fatalf("%s derived the SAME key as the base call, so one of the two "+
					"writes would be silently dropped", name)
			}
		})
	}
}

// VALUES MUST NOT BE CONFUSABLE ACROSS FIELD BOUNDARIES. Without length prefixing,
// moving a character from one argument to the next would produce the same key, and two
// genuinely different messages would deduplicate against each other.
func TestIdempotencyKeyCannotBeConfusedAcrossFields(t *testing.T) {
	fn := keyFuncFor(t, "send_message")

	a, _ := fn(map[string]any{"channel_uuid": "ab", "text": "c"})
	b, _ := fn(map[string]any{"channel_uuid": "a", "text": "bc"})
	if a == b {
		t.Fatal("two different calls hash alike because the parts are not delimited " +
			"unambiguously; one of the two writes would be dropped as a duplicate")
	}
}

// The key must be derived from the value the EXECUTOR receives. Hashing the raw JSON
// instead would make 5 and 5.0 look like different calls that then perform the
// identical write.
func TestIdempotencyKeyUsesTheCoercedArgumentValue(t *testing.T) {
	fn := keyFuncFor(t, "create_table_row")

	whole, _ := fn(map[string]any{"table_uuid": "t-1", "values": float64(5)})
	same, _ := fn(map[string]any{"table_uuid": "t-1", "values": "5"})
	if whole != same {
		t.Fatalf("a JSON number and the string the executor actually receives derive "+
			"different keys (%q vs %q), so a retry would not be recognised", whole, same)
	}
}

// UNDECLARED ARGUMENTS MUST NOT CHANGE IDENTITY. A client adding a field the executor
// ignores must not turn a retry into a second write.
func TestIdempotencyKeyIgnoresUndeclaredArguments(t *testing.T) {
	fn := keyFuncFor(t, "send_message")

	plain, _ := fn(map[string]any{"channel_uuid": "c-1", "text": "hi"})
	noisy, _ := fn(map[string]any{"channel_uuid": "c-1", "text": "hi", "client_nonce": "abc123"})
	if plain != noisy {
		t.Fatal("an argument the tool does not declare changed the call's identity, so a " +
			"client adding a nonce would defeat deduplication entirely")
	}
}

// Read-only and naturally idempotent behaviours need no key, and Register expects nil
// for them rather than an unused function.
func TestNoKeyIsGeneratedWhenNoneIsNeeded(t *testing.T) {
	def, _ := staticToolDef("read_doc")
	for name, b := range map[string]ToolBehaviour{
		"read-only":            {ReadOnly: true, Idempotent: true},
		"idempotent write":     {ReadOnly: false, Idempotent: true},
		"read, not idempotent": {ReadOnly: true, Idempotent: false},
	} {
		t.Run(name, func(t *testing.T) {
			if idempotencyKeyFor(def, b) != nil {
				t.Fatal("a key function was generated for a behaviour that needs none")
			}
		})
	}
}

// Every non-idempotent bridged write must actually get a key function, or Register would
// refuse it at startup and the server would not boot.
func TestEveryNonIdempotentBridgedWriteCanDeriveAKey(t *testing.T) {
	for _, b := range bridgedTools {
		if !b.Behaviour.needsIdempotencyKey() {
			continue
		}
		def, ok := staticToolDef(b.Tool)
		if !ok {
			t.Errorf("%q is bridged but not in ai.ToolRegistry", b.Tool)
			continue
		}
		fn := idempotencyKeyFor(def, b.Behaviour)
		if fn == nil {
			t.Errorf("%q needs a deduplication key and got none; Register would refuse it "+
				"and the server would fail to start", b.Tool)
			continue
		}
		// Derives something non-empty from a realistic argument set.
		args := map[string]any{}
		for _, p := range def.Parameters {
			args[p.Name] = "x"
		}
		key, err := fn(args)
		if err != nil || strings.TrimSpace(key) == "" {
			t.Errorf("%q derived no key (err=%v, key=%q); an empty key makes every call "+
				"a duplicate of the first", b.Tool, err, key)
		}
	}
}

// An empty key means "nothing to deduplicate", so ClaimWrite must pass it through
// rather than touching the store. Callers rely on this to avoid a branch of their own.
func TestClaimWriteIsANoopWithoutAKey(t *testing.T) {
	got := ClaimWrite(nil, &ToolSpec{Name: "x"}, ToolCallContext{}, "client", "")
	if got.Outcome != WriteProceed {
		t.Fatalf("an empty key should proceed, got %s (%s)", got.Outcome, got.Reason)
	}
	if got.ClaimID != "" {
		t.Fatal("no claim should be recorded when there is nothing to deduplicate")
	}
}

// A call with no accountable person must not be claimed anonymously — the row is
// attributed to the human whose credential authorised it.
func TestClaimWriteRefusesWithoutAPrincipal(t *testing.T) {
	got := ClaimWrite(nil, &ToolSpec{Name: "x"}, ToolCallContext{PrincipalUserID: "not-a-uuid"},
		"client", "some-key")
	if got.Outcome != WriteRefused {
		t.Fatalf("expected a refusal without a principal, got %s", got.Outcome)
	}
}

// THE RATCHET. A claimed write must be settled on BOTH the success and the failure path.
// An unsettled claim is read as "already applied" by every later retry, so forgetting
// the failure path would turn a failed write into one that is permanently reported as
// having succeeded — the worst outcome available, because it is silent.
func TestGovernedPathClaimsAndAlwaysSettles(t *testing.T) {
	raw, err := os.ReadFile("../../controllers/MCP/governed.go")
	if err != nil {
		t.Fatalf("read governed.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

	claim := strings.Index(src, "ClaimWrite(")
	if claim < 0 {
		t.Fatal("the governed path no longer reserves a write against duplicates; a " +
			"client retry after a lost response would perform the write twice")
	}
	handler := strings.Index(src, "decision.Spec.Handler(")
	if handler < 0 {
		t.Fatal("could not find the handler invocation; this ratchet has gone stale")
	}
	if claim > handler {
		t.Error("the write is reserved AFTER the handler runs, which leaves two concurrent " +
			"retries both executing; the reservation is what separates them")
	}

	settle := strings.Index(src, "SettleWrite(")
	if settle < 0 {
		t.Fatal("a claimed write is never settled. Every later retry of that call would be " +
			"told it had already been applied, whether or not it ever ran")
	}
	if settle < handler {
		t.Error("the claim is settled before the handler returns, so the recorded outcome " +
			"cannot reflect what actually happened")
	}

	// Settling must not sit behind an error check, which would skip the failure path.
	between := src[handler:settle]
	if strings.Contains(between, "if err != nil") {
		t.Error("SettleWrite is only reached when the handler succeeded. A failed write " +
			"would leave its key claimed forever, and every retry would be told the " +
			"write had already been applied")
	}
}

// The abandoned-execution reclaimer must exist and be scheduled, or a server killed
// mid-write blocks that exact call permanently.
func TestAbandonedExecutionsAreReclaimed(t *testing.T) {
	model, err := os.ReadFile("../../models/postgres/PendingAction/pendingActionModel.go")
	if err != nil {
		t.Fatalf("read pendingActionModel.go: %v", err)
	}
	if !strings.Contains(string(model), "status='executing'") ||
		!strings.Contains(string(model), "ReclaimAbandonedExecutions") {
		t.Error("nothing moves a row out of 'executing' except the process that claimed it. " +
			"A server killed mid-write leaves the key claimed forever, and 'executing' is " +
			"read as 'already applied' — so every retry is told a write succeeded that " +
			"never ran")
	}

	main, err := os.ReadFile("../../cmd/server/main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(main), "StartPendingActionHousekeeping()") {
		t.Error("the housekeeping sweep is not started, so nothing reclaims abandoned " +
			"executions or expires overdue proposals")
	}
}

// Guards against the registry drifting under the generated key functions.
func TestBridgedToolsExistInTheStaticRegistry(t *testing.T) {
	for _, b := range bridgedTools {
		if _, ok := staticToolDef(b.Tool); !ok {
			t.Errorf("%q is bridged but is not a compile-time entry in ai.ToolRegistry", b.Tool)
		}
	}
	// Sanity: the registry is populated at all, so the assertions above are not vacuous.
	if len(ai.ToolRegistry) == 0 {
		t.Fatal("ai.ToolRegistry is empty; these tests would pass vacuously")
	}
}

// modelSource returns the pending-action model source, comments stripped.
func modelSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../models/postgres/PendingAction/pendingActionModel.go")
	if err != nil {
		t.Fatalf("read pendingActionModel.go: %v", err)
	}
	return regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")
}

// A FAILED WRITE MUST BECOME RETRYABLE, and this is the assertion that would have caught
// the bug it exists for.
//
// A settled row keeps its idempotency key: the unique index is on the key alone, not the
// key plus a status. So a row left 'failed' by a transient error blocks the insert forever
// while truthfully reporting that nothing took effect — which made that exact call
// permanently unrepeatable, and made this file's "a failed write frees the key" claim
// false. Nothing in the Go code read wrong; it only shows up when the statements run.
func TestAFailedWriteCanBeRetried(t *testing.T) {
	claim, err := os.ReadFile("claim.go")
	if err != nil {
		t.Fatalf("read claim.go: %v", err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(claim), " ")

	// Two separate facts, because the store seam split them: the ORDERING lives in the
	// decision function, the PERSISTENCE in the live store. Asserting only the first would
	// pass while the live store was wired to nothing, and every fake-store test with it.
	if !strings.Contains(src, "store.Retake(") {
		t.Fatal("ClaimWrite never retakes a key whose previous attempt did not complete. " +
			"A settled row keeps its key, so one transient failure would make that exact " +
			"call permanently unrepeatable: every retry conflicts on the insert, reads " +
			"'failed', and is refused.")
	}
	if !strings.Contains(src, "pendingModels.RetakeIdempotencyKey") {
		t.Error("the live claim store no longer wires Retake to the model's " +
			"RetakeIdempotencyKey, so the retake branch would be a no-op in production " +
			"while every unit test using a fake store still passed")
	}

	// The retake must be attempted BEFORE falling through to the existing-state read,
	// or the refusal short-circuits it and nothing is ever retaken.
	retake := strings.Index(src, "store.Retake(")
	read := strings.Index(src, "store.Existing(")
	if read >= 0 && read < retake {
		t.Error("ClaimWrite reads the existing state before attempting a retake, so a " +
			"retryable failure is answered as a refusal and the retake is unreachable")
	}
}

// THE EXCLUSIONS ARE THE SAFETY PROPERTY. Retaking the wrong state would be worse than
// not retaking at all: re-running an applied write duplicates it, and re-running a
// REJECTED one overrides a human's refusal.
func TestOnlyNonAppliedStatesAreRetakeable(t *testing.T) {
	src := modelSource(t)

	at := strings.Index(src, "func RetakeIdempotencyKey")
	if at < 0 {
		t.Fatal("RetakeIdempotencyKey is gone; a failed write would be unrepeatable again")
	}
	body := src[at:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}

	// Exactly these two, and the guard must be in the WHERE clause rather than in Go.
	if !strings.Contains(body, "status IN ('failed','expired')") {
		t.Error("the retake is not guarded to ('failed','expired') in SQL. The guard must " +
			"be in the WHERE clause so the check and the update are one atomic statement — " +
			"reading the status first and updating after is a race two retries can both win")
	}
	for _, forbidden := range []string{"'executed'", "'rejected'", "'pending'", "'executing'"} {
		// Any of these appearing inside the retake's guard would be a serious error.
		if strings.Contains(body, "status IN ('failed','expired',"+forbidden) ||
			strings.Contains(body, forbidden+",'failed'") {
			t.Errorf("the retake guard includes %s. Retaking that state either duplicates "+
				"an applied write or overrides a human's refusal", forbidden)
		}
	}

	// created_at MUST be reset. The abandoned-execution reclaimer selects on created_at,
	// so retaking a two-hour-old row without moving it lets the reclaimer free the row
	// again immediately — while the retry is still running.
	if !strings.Contains(body, "created_at=NOW()") {
		t.Error("the retake does not reset created_at, so the abandoned-execution reclaimer " +
			"would free the row immediately after it is retaken, mid-execution")
	}
	// The previous attempt's outcome must be cleared, or a successful retry still reads as
	// having failed.
	for _, clear := range []string{"result=NULL", "error=NULL", "resolved_at=NULL", "resolved_by=NULL"} {
		if !strings.Contains(body, clear) {
			t.Errorf("the retake does not clear %s; a successful retry would still carry the "+
				"failed attempt's outcome", clear)
		}
	}
}

// The claim insert and the retake must both name the partial index's predicate, or
// Postgres cannot infer the index and the statement fails at runtime — on the first
// non-idempotent write, in front of a customer.
func TestTheClaimInsertMatchesThePartialIndex(t *testing.T) {
	src := modelSource(t)

	if !strings.Contains(src, "ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING") {
		t.Error("the claim insert's ON CONFLICT clause no longer matches the partial unique " +
			"index idx_ai_pending_actions_idem, which is `(idempotency_key) WHERE " +
			"idempotency_key IS NOT NULL`. Postgres infers the index from the conflict " +
			"target and its predicate; a mismatch is a runtime error on the first " +
			"non-idempotent write, not a compile error.")
	}

	// A blank key must be refused rather than inserted: NULL keys are exempt from the
	// unique index, so an unkeyed claim row would be inserted afresh on every call and
	// deduplicate nothing.
	for _, fn := range []string{"func ClaimIdempotencyKey", "func RetakeIdempotencyKey"} {
		at := strings.Index(src, fn)
		if at < 0 {
			t.Fatalf("%s is gone; this ratchet has gone stale", fn)
		}
		body := src[at:]
		if end := strings.Index(body, "\nfunc "); end > 0 {
			body = body[:end]
		}
		if !strings.Contains(body, "TrimSpace(idempotencyKey) ==") {
			t.Errorf("%s does not refuse a blank key. A NULL key is exempt from the unique "+
				"index, so it would insert an unclaimable row on every call and "+
				"deduplicate nothing", fn)
		}
	}
}

// Finalize is guarded on 'executing', which is the state a claim lands in. If the claim
// stopped landing there, every settle would silently affect no rows and every later retry
// would be told the write had already been applied.
func TestClaimLandsInTheStateFinalizeExpects(t *testing.T) {
	src := modelSource(t)

	if !strings.Contains(src, "VALUES ($1,$2,$3,$4,$5,$6,$7,'executing',$8,$9)") {
		t.Error("the claim no longer inserts directly as 'executing'. It must, for two " +
			"reasons: Finalize is guarded on that state, and a row briefly in 'pending' " +
			"would be picked up by the open-actions query and shown to a human as " +
			"something to approve")
	}
	at := strings.Index(src, "func Finalize")
	if at < 0 {
		t.Fatal("Finalize is gone; this ratchet has gone stale")
	}
	body := src[at:]
	if end := strings.Index(body, "\nfunc "); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "status='executing'") {
		t.Error("Finalize is no longer guarded on 'executing'; it must agree with the state " +
			"the claim inserts, or settling silently does nothing")
	}
}
