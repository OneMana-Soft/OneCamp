package helpers

import (
	"context"
	"testing"
	"time"
)

// The work a write starts on its own goroutine takes the write's backdated
// time and nothing else of its request (V-871): not who asked, and no
// deadline, so the notices of a seeded message are as of it and outlive the
// request; with no such time it is a plain background context.
func TestImportTimeOnlyCarriesTheTimeAlone(t *testing.T) {
	at := time.Date(2026, 11, 16, 22, 1, 0, 0, time.UTC)
	req, cancel := context.WithCancel(context.WithValue(WithImportTimestamp(context.Background(), at), UserInfoContextKey, "someone"))
	cancel()
	got := ImportTimeOnly(req)
	if t2 := GetImportTimestamp(got); t2 == nil || !t2.Equal(at) {
		t.Errorf("import time %v, want %v", t2, at)
	}
	if got.Value(UserInfoContextKey) != nil || got.Err() != nil {
		t.Errorf("carried the request's asker (%v) or its end (%v)", got.Value(UserInfoContextKey), got.Err())
	}
	for _, ctx := range []context.Context{context.Background(), nil} {
		if live := ImportTimeOnly(ctx); live == nil || GetImportTimestamp(live) != nil {
			t.Errorf("ImportTimeOnly(%v) = %v, want a background context with no import time", ctx, live)
		}
	}
}
