package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The broker reads only a 200 with {"result": ...}; anything else it treats
// as no answer. Each of these is refused before any lookup.
func TestTheBrokerIsAnsweredInItsOwnTerms(t *testing.T) {
	for name, body := range map[string]string{
		"not json":          "{",
		"a publish":         `{"username":"user_00000000-0000-4000-8000-000000000001","topic":"public/userStatus","action":"publish"}`,
		"a wildcard":        `{"username":"user_00000000-0000-4000-8000-000000000001","topic":"message/#","action":"subscribe"}`,
		"no username":       `{"topic":"public/userStatus","action":"subscribe"}`,
		"an oversized body": `{"username":"` + strings.Repeat("a", 8<<10) + `"}`,
	} {
		w := httptest.NewRecorder()
		AuthorizeMqtt(w, httptest.NewRequest(http.MethodPost, "/internal/mqtt/authorize", strings.NewReader(body)))
		var got struct {
			Result string `json:"result"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Result != "deny" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
}
