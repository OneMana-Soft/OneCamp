package business

import (
	"crypto/hmac"
	"crypto/sha256"
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
	s, err := open(b64.EncodeToString(body) + "." + b64.EncodeToString(m.Sum(nil)))
	if err != nil || s.Ref.ID != "e1" {
		t.Fatalf("a card sealed under the earlier key: %+v %v", s, err)
	}
}
