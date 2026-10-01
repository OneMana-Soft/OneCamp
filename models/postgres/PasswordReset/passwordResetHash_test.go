package models

import (
	"strings"
	"testing"
)

// The stored form must not be the token.
//
// This is the whole change: plaintext, this table was a list of working skeleton
// keys for every reset in flight, replayable by anyone who could read it.
func TestHashResetTokenDoesNotStoreTheToken(t *testing.T) {
	tok := "74b56190d8461525a1f8c0d3e9b27a4c5d6e8f019283746556677889aabbccdd"
	got := HashResetToken(tok)

	if got == tok {
		t.Fatal("the stored form IS the token")
	}
	if strings.Contains(got, tok[:16]) {
		t.Fatal("the stored form leaks a prefix of the token")
	}
	if len(got) != 64 {
		t.Fatalf("expected 64 hex chars of SHA-256, got %d", len(got))
	}
	if strings.Trim(got, "0123456789abcdef") != "" {
		t.Fatalf("not lowercase hex: %q", got)
	}
}

// Validation looks a token up by its hash, so the same input must always give the
// same output or every reset link breaks.
func TestHashResetTokenIsStable(t *testing.T) {
	const tok = "abc123"
	if HashResetToken(tok) != HashResetToken(tok) {
		t.Fatal("not deterministic, so no link would ever validate twice")
	}
	if HashResetToken("a") == HashResetToken("b") {
		t.Fatal("different tokens collided")
	}
}

// NOT normalised, unlike HashRecoveryCode.
//
// That one upper-cases and strips separators because people copy recovery codes off
// paper. This token travels in a URL, machine to machine, and folding case would
// throw away distinctions the generator relies on — and would silently accept a
// token an attacker only guessed the case-insensitive form of.
func TestHashResetTokenIsCaseSensitive(t *testing.T) {
	if HashResetToken("deadbeef") == HashResetToken("DEADBEEF") {
		t.Fatal("case was folded, so the token space is smaller than the generator thinks")
	}
	if HashResetToken(" abc ") == HashResetToken("abc") {
		t.Fatal("whitespace was stripped, which no URL-borne token needs")
	}
}

// Pinned against a real SHA-256 vector, because `make reset-link` on a customer's
// machine hashes with Postgres's encode(sha256(x::bytea),'hex') instead of calling
// this function. The two must agree byte for byte, or a link the operator reads out
// to a customer will not validate and nobody will know why.
//
// The vector below was produced by BOTH openssl and the postgres 12 image, so this
// fails if either this function stops being plain SHA-256 hex of the raw bytes, or
// somebody adds normalisation to it later.
func TestHashResetTokenMatchesPostgresSHA256(t *testing.T) {
	const input = "onecamp"
	const want = "4d77c6f6fed8d5527368fe1a744c224fd0300505a6306cfbde31b29f27dfb868"

	if got := HashResetToken(input); got != want {
		t.Fatalf("Go and Postgres would disagree:\n  go: %s\n  pg: %s", got, want)
	}
}
