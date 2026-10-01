package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The administrative reset refuses to target the acting admin's own account.
//
// WHY THIS IS THE PROPERTY WORTH A TEST. DisableTOTP demands a valid code precisely so that a hijacked
// session cannot remove the protection the second factor exists to provide. An administrative reset that
// accepted its own caller's id would hand that bypass straight back: an attacker who reached an admin
// account would post their own uuid and be rid of the requirement in one request, with the code check
// still sitting there looking like it meant something.
//
// It is a one-line guard, which is exactly why it is worth pinning — a future refactor that moves the
// existence lookup above it, or drops it while "simplifying" the signature, produces a working endpoint
// with a silent hole and no failing test to say so.
// ONE TEST RATHER THAN TWO, because the second thing it asserts is the ordering, and a separate test
// for that could never report it: postgresInit.DBConn is `&DB{}` with a nil SqlDB in a unit test, so a
// self-check moved below the status read panics on nil — and a panic in whichever test ran first killed
// the binary before the test with the explanatory message got to run. Verified by mutation: splitting
// these produced `nil pointer dereference` as the only diagnostic. Merged, the recover below is always
// the thing that reports it.
func TestAdminResetTOTPRefusesSelfTargeting(t *testing.T) {
	// The ordering half. A self-check placed after the existence lookup would reach a nil connection
	// here, so the absence of a panic is the proof that nothing is read on behalf of a request that was
	// always going to be refused. It matters for the audit trail too: the controller records a refusal,
	// and one that had already touched the user's row is a different event from one that never got there.
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("AdminResetTOTP reached the database before refusing a self-targeted reset "+
				"(panicked on the nil test connection: %v). The self-check must come first", recovered)
		}
	}()

	admin := uuid.New()

	wasEnrolled, err := AdminResetTOTP(context.Background(), admin, admin)

	if !errors.Is(err, ErrTOTPSelfResetRefused) {
		t.Fatalf("a self-targeted reset was not refused (err=%v). Anyone holding a stolen admin "+
			"session could strip their own second factor without proving possession of it", err)
	}
	if wasEnrolled {
		t.Error("a refused reset must not report that a factor was removed; the audit summary is " +
			"written from this value")
	}
}

// The sentinel is comparable with errors.Is and says what to do next.
//
// The controller maps this exact sentinel to 403 with a machine-readable code, so an identity check by
// message text would tie the HTTP status to the wording of a user-facing sentence.
func TestErrTOTPSelfResetRefusedIsActionable(t *testing.T) {
	if ErrTOTPSelfResetRefused == nil {
		t.Fatal("the sentinel is nil")
	}
	// Names the way out. A refusal that only says "not allowed" sends an admin who has genuinely lost
	// their phone to support, when another administrator or a recovery code would solve it in a minute.
	for _, want := range []string{"another administrator", "recovery code"} {
		if !strings.Contains(ErrTOTPSelfResetRefused.Error(), want) {
			t.Errorf("the refusal should mention %q so the reader knows the remedy: %q",
				want, ErrTOTPSelfResetRefused.Error())
		}
	}
}
