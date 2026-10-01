package controllers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"testing"
	"time"
)

// TestVerifyResendWebhookSignature covers all the rejection paths and the
// happy path for the Svix signature scheme used by Resend.
func TestVerifyResendWebhookSignature(t *testing.T) {
	rawKey := []byte("super-secret-bytes")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(rawKey)
	id := "msg_2NfqDZ"
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(`{"type":"email.bounced"}`)

	signed := id + "." + ts + "."
	mac := hmac.New(sha256.New, rawKey)
	mac.Write([]byte(signed))
	mac.Write(body)
	sig := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))

	t.Run("happy path", func(t *testing.T) {
		if !verifyResendWebhookSignature(secret, id, ts, sig, body) {
			t.Fatalf("expected valid signature to verify")
		}
	})

	t.Run("missing headers rejected", func(t *testing.T) {
		if verifyResendWebhookSignature(secret, "", ts, sig, body) {
			t.Fatalf("missing svix-id should be rejected")
		}
		if verifyResendWebhookSignature(secret, id, "", sig, body) {
			t.Fatalf("missing svix-timestamp should be rejected")
		}
		if verifyResendWebhookSignature(secret, id, ts, "", body) {
			t.Fatalf("missing svix-signature should be rejected")
		}
	})

	t.Run("replay window rejected", func(t *testing.T) {
		oldTs := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
		oldSigned := id + "." + oldTs + "."
		mac2 := hmac.New(sha256.New, rawKey)
		mac2.Write([]byte(oldSigned))
		mac2.Write(body)
		oldSig := "v1," + base64.StdEncoding.EncodeToString(mac2.Sum(nil))
		if verifyResendWebhookSignature(secret, id, oldTs, oldSig, body) {
			t.Fatalf("expected stale timestamp to be rejected")
		}
	})

	t.Run("body tampering rejected", func(t *testing.T) {
		if verifyResendWebhookSignature(secret, id, ts, sig, []byte(`{}`)) {
			t.Fatalf("expected tampered body to be rejected")
		}
	})

	t.Run("wrong secret rejected", func(t *testing.T) {
		wrong := "whsec_" + base64.StdEncoding.EncodeToString([]byte("other-key"))
		if verifyResendWebhookSignature(wrong, id, ts, sig, body) {
			t.Fatalf("expected wrong secret to be rejected")
		}
	})

	t.Run("multiple signatures, one valid", func(t *testing.T) {
		multi := "v1,bogus v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if !verifyResendWebhookSignature(secret, id, ts, multi, body) {
			t.Fatalf("expected at-least-one-match to pass")
		}
	})
}

// TestValidHHMM covers the HH:MM input validator used by the prefs endpoint.
func TestValidHHMM(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"00:00", true},
		{"23:59", true},
		{"22:00", true},
		{"07:00", true},
		{"24:00", false},
		{"23:60", false},
		{"7:00", false}, // requires 2-digit hour
		{"07:0", false}, // requires 2-digit minute
		{"abc", false},
		{"", false},
		{"7:00 PM", false},
		{"-1:00", false},
	}
	for _, c := range cases {
		got := validHHMM(c.in)
		if got != c.want {
			t.Errorf("validHHMM(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
