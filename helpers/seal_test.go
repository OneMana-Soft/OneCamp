package helpers

import (
	"errors"
	"strings"
	"testing"
)

// A sealed body opens only under the purpose it was sealed for, whole: a
// changed byte, another purpose's key or no secret opens nothing.
func TestSealOpensOnlyItsOwnPurpose(t *testing.T) {
	t.Setenv("JWT_SECRET", "a server's secret")
	type body struct {
		Room string `json:"r"`
		Exp  int64  `json:"e"`
	}
	token, err := Seal("onecamp.test.v1", body{Room: "room-1", Exp: 42})
	if err != nil {
		t.Fatal(err)
	}
	var got body
	if err := Unseal("onecamp.test.v1", token, &got); err != nil || got != (body{Room: "room-1", Exp: 42}) {
		t.Fatalf("Unseal = %+v, %v", got, err)
	}
	if strings.ContainsAny(token, "/+=?#") {
		t.Errorf("a sealed token %q isn't safe in an address's path", token)
	}
	b, sig, _ := strings.Cut(token, ".")
	for name, bad := range map[string]string{
		"none":            "",
		"no signature":    b,
		"a changed body":  "x" + b[1:] + "." + sig,
		"a changed seal":  b + "." + sig[:len(sig)-2] + "AA",
		"not base64":      "!!." + sig,
		"another's token": token + "x",
	} {
		if err := Unseal("onecamp.test.v1", bad, &got); !errors.Is(err, ErrSealBroken) {
			t.Errorf("%s: Unseal = %v, want ErrSealBroken", name, err)
		}
	}
	if err := Unseal("onecamp.other.v1", token, &got); !errors.Is(err, ErrSealBroken) {
		t.Errorf("another purpose: Unseal = %v, want ErrSealBroken", err)
	}
	t.Setenv("JWT_SECRET", "")
	if _, err := Seal("onecamp.test.v1", body{}); !errors.Is(err, ErrNoSealKey) {
		t.Errorf("no secret: Seal = %v, want ErrNoSealKey", err)
	}
	if err := Unseal("onecamp.test.v1", token, &got); !errors.Is(err, ErrNoSealKey) {
		t.Errorf("no secret: Unseal = %v, want ErrNoSealKey", err)
	}
}
