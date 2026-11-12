package helpers

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The storefront's address: an operator's own (a staging storefront, or "off"),
// else the one the build names.
func TestStorefrontURLIsTheOperatorsOrTheDefault(t *testing.T) {
	const env, def = "ONECAMP_TEST_STOREFRONT_URL", "https://backend.example/onecamp/x"
	t.Setenv(env, "")
	if got := StorefrontURL(env, def); got != def {
		t.Errorf("unset: %q, want the default", got)
	}
	t.Setenv(env, "  https://staging.example/onecamp/x \n")
	if got := StorefrontURL(env, def); got != "https://staging.example/onecamp/x" {
		t.Errorf("set: %q, want the operator's, trimmed", got)
	}
}

// One way to ask the storefront: a GET for JSON, a body read to 64 KB at
// most, and a failure that names the address and nothing the answer said.
func TestGetStorefrontAsksForJSONAndReadsAtMost64KB(t *testing.T) {
	var method, accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, accept = r.Method, r.Header.Get("Accept")
		if r.URL.Path == "/down" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("upstream said something"))
			return
		}
		_, _ = w.Write(bytes.Repeat([]byte("a"), 100<<10))
	}))
	ctx := context.Background()
	size := func(r io.Reader) (int, error) {
		b, err := io.ReadAll(r)
		return len(b), err
	}
	n, err := GetStorefront(ctx, srv.URL+"/big", size)
	if err != nil || n != 64<<10 || method != http.MethodGet || accept != "application/json" {
		t.Fatalf("read %d bytes, err %v, %s with Accept %q; want 65536, nil, GET, application/json", n, err, method, accept)
	}
	if _, err := GetStorefront(ctx, srv.URL+"/down", size); err == nil || !strings.HasSuffix(err.Error(), "answered 502") || strings.Contains(err.Error(), "upstream said") {
		t.Errorf("a 502: %v, want \"<address> answered 502\" and none of its body", err)
	}
	if _, err := GetStorefront(ctx, srv.URL+"/big", func(io.Reader) (int, error) { return 0, io.ErrUnexpectedEOF }); err != io.ErrUnexpectedEOF {
		t.Errorf("the parse's own error: %v", err)
	}
	gone := srv.URL
	srv.Close()
	if _, err := GetStorefront(ctx, gone+"/big", size); err == nil || !strings.HasPrefix(err.Error(), "could not reach "+gone) {
		t.Errorf("unreachable: %v, want \"could not reach <address>\"", err)
	}
}
