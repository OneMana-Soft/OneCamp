package business

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// A card sealed before its key came from helpers.PurposeKey still opens: the
// key is the same HMAC of "onecamp.cards.v1" under the server's secret, so
// cards already on people's screens keep working across the deploy.
func TestACardSealedUnderTheEarlierKeyStillOpens(t *testing.T) {
	t.Setenv("JWT_SECRET", "a server's secret")
	earlier := hmac.New(sha256.New, []byte("a server's secret"))
	earlier.Write([]byte("onecamp.cards.v1"))
	body, err := json.Marshal(sealed{Ref: CardRef{Host: HostEphemeral, ID: "e1"}, Actions: []string{"approve"}, Exp: now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, earlier.Sum(nil))
	m.Write(body)
	s, err := open(base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)))
	if err != nil || s.Ref.ID != "e1" {
		t.Fatalf("a card sealed under the earlier key: %+v %v", s, err)
	}
}

// A card's token keeps its format byte for byte: one sealed before the seal
// moved to helpers.Seal (base64url body, ".", base64url HMAC of the body under
// the cards key) still opens, so cards already on screens keep working.
func TestACardTokenFromBeforeTheSharedSealStillOpens(t *testing.T) {
	t.Setenv("JWT_SECRET", "a fixed secret")
	const vector = "eyJyZWYiOnsiaCI6ImVwaGVtZXJhbCIsInIiOiJlMSJ9LCJvIjp7ImsiOiJidWlsdGluIiwiaSI6InBvbGwifSwiYSI6WyJhcHByb3ZlIl0sInUiOlsidS0xIl0sImUiOjQxMDI0NDQ4MDB9.Fz8gBH0q6tRluHuTzfA6AaBITK7L79d-dqeOxrkYZzA"
	s, err := open(vector)
	if err != nil || s.Ref != (CardRef{Host: HostEphemeral, ID: "e1"}) || s.Owner != (Owner{Kind: OwnerBuiltin, ID: "poll"}) ||
		len(s.Actions) != 1 || s.Actions[0] != "approve" || len(s.Actors) != 1 || s.Actors[0] != "u-1" || s.Exp != 4102444800 {
		t.Fatalf("the earlier card's token: %+v %v", s, err)
	}
	if _, err := open(vector[:len(vector)-1] + "Q"); err != ErrBadToken {
		t.Errorf("a changed seal: %v, want ErrBadToken", err)
	}
	t.Setenv("JWT_SECRET", "")
	if _, err := open(vector); err != ErrNoSecret {
		t.Errorf("no secret: %v, want ErrNoSecret", err)
	}
}
