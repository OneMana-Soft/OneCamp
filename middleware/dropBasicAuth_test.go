package middleware

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A Basic Authorization never reaches the handlers, in any case of its scheme;
// a Bearer one, which every route that reads Authorization takes, is kept.
func TestABasicAuthorizationIsDroppedAndABearerIsKept(t *testing.T) {
	basic := base64.StdEncoding.EncodeToString([]byte("oc_token:"))
	for _, c := range []struct{ name, sent, want string }{
		{"Basic", "Basic " + basic, ""},
		{"basic, in another case", "bAsIc " + basic, ""},
		{"Bearer", "Bearer oc_token", "Bearer oc_token"},
		{"none", "", ""},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/docs", nil)
		if c.sent != "" {
			req.Header.Set("Authorization", c.sent)
		}
		var got string
		reached := false
		DropBasicAuth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			reached = true
			got = r.Header.Get("Authorization")
		})).ServeHTTP(httptest.NewRecorder(), req)
		if !reached || got != c.want {
			t.Errorf("%s: reached %v with Authorization %q, want %q", c.name, reached, got, c.want)
		}
	}
}
