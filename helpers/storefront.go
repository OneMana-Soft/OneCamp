package helpers

// The storefront (onemana.dev) answers the few questions a self-hosted server
// asks it: the current releases (business/Updates) and the prices
// (business/Plan). Each is asked only when an admin opens the screen that
// shows it, and each keeps its own parse and cache; this is the one way they
// ask. Nothing about the workspace goes with the GET, and a failure names the
// address and the status, never what the answer said.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	storefrontTimeout = 8 * time.Second
	// The most of an answer read: both answers are a few hundred bytes.
	storefrontMaxBody = 64 << 10
)

// StorefrontURL is the address in the env variable when an operator set one
// (a staging storefront, or a word a caller reads as "don't ask"), else def.
func StorefrontURL(env, def string) string {
	if u := strings.TrimSpace(os.Getenv(env)); u != "" {
		return u
	}
	return def
}

// GetStorefront GETs from for JSON and hands parse at most 64 KB of a 200's
// body. Not reached in 8 s, or any other status, is an error naming from.
func GetStorefront[T any](ctx context.Context, from string, parse func(io.Reader) (T, error)) (T, error) {
	var none T
	ctx, cancel := context.WithTimeout(ctx, storefrontTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, from, nil)
	if err != nil {
		return none, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return none, fmt.Errorf("could not reach %s: %w", from, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return none, fmt.Errorf("%s answered %d", from, resp.StatusCode)
	}
	return parse(io.LimitReader(resp.Body, storefrontMaxBody))
}
