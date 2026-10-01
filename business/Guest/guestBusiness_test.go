package business

import (
	"strings"
	"testing"
)

func TestSanitizeGuestName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trim", "  Alice  ", "Alice"},
		{"empty", "   ", ""},
		{"control chars stripped", "A\x00l\x07ice", "Alice"},
		{"collapse whitespace", "Alice   Smith", "Alice Smith"},
		{"newlines become space", "Alice\nSmith", "Alice Smith"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeGuestName(c.in); got != c.want {
				t.Fatalf("sanitizeGuestName(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSanitizeGuestNameLengthCap(t *testing.T) {
	long := strings.Repeat("a", maxGuestNameLen+25)
	got := sanitizeGuestName(long)
	if len([]rune(got)) > maxGuestNameLen {
		t.Fatalf("name not capped: got %d runes, want <= %d", len([]rune(got)), maxGuestNameLen)
	}
}

func TestHashTokenIsDeterministicAndSized(t *testing.T) {
	a := hashToken("abc")
	b := hashToken("abc")
	c := hashToken("abd")
	if len(a) != 32 {
		t.Fatalf("sha-256 hash should be 32 bytes, got %d", len(a))
	}
	if string(a) != string(b) {
		t.Fatal("hashToken must be deterministic for the same input")
	}
	if string(a) == string(c) {
		t.Fatal("hashToken must differ for different inputs")
	}
}

func TestRandomTokenUniqueAndUrlSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := randomToken()
		if err != nil {
			t.Fatalf("randomToken err: %v", err)
		}
		if tok == "" {
			t.Fatal("randomToken returned empty")
		}
		if strings.ContainsAny(tok, "+/=") {
			t.Fatalf("randomToken not URL-safe: %q", tok)
		}
		if seen[tok] {
			t.Fatalf("randomToken collision: %q", tok)
		}
		seen[tok] = true
	}
}

func TestIsMeetingRoom(t *testing.T) {
	if !IsMeetingRoom(MeetingRoomPrefix + "abc") {
		t.Fatal("expected meet- room to be recognized")
	}
	if IsMeetingRoom("550e8400-e29b-41d4-a716-446655440000") {
		t.Fatal("a channel UUID room must not be treated as a meeting room")
	}
	if IsMeetingRoom("uuid1 uuid2") {
		t.Fatal("a DM room must not be treated as a meeting room")
	}
}
