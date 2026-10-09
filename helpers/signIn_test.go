package helpers

import "testing"

// AUTH_EMAIL_DISABLED=true turns passwords off; the sign-in page used to read
// it backwards, so "true" left them on.
func TestTurningPasswordsOffMeansTrue(t *testing.T) {
	for value, off := range map[string]bool{"true": true, "TRUE": true, "1": true, "yes": true, "": false, "false": false, "0": false} {
		t.Setenv("AUTH_EMAIL_DISABLED", value)
		if got := PasswordSignInOff(); got != off {
			t.Errorf("AUTH_EMAIL_DISABLED=%q: off %v, want %v", value, got, off)
		}
	}
}

// email_verified arrives as a boolean, or from some providers as "true".
func TestAClaimSaysTrueEitherWay(t *testing.T) {
	for _, c := range []struct {
		claim any
		want  bool
	}{{true, true}, {"true", true}, {"TRUE", true}, {" true ", true}, {false, false}, {"false", false}, {nil, false}, {1, false}} {
		if got := TrueClaim(c.claim); got != c.want {
			t.Errorf("%#v: got %v, want %v", c.claim, got, c.want)
		}
	}
}
