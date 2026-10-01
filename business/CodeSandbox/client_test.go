package codesandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewHTTPRunner_NilOnEmptyURL(t *testing.T) {
	if NewHTTPRunner("", "tok") != nil {
		t.Fatal("empty URL must yield a nil runner (=> unavailable)")
	}
	if NewHTTPRunner("http://x/run", "tok") == nil {
		t.Fatal("a configured URL must yield a runner")
	}
}

func TestHTTPRunner_RoundTrip(t *testing.T) {
	var gotToken string
	var gotJob Job
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Runner-Token")
		_ = json.NewDecoder(r.Body).Decode(&gotJob)
		_ = json.NewEncoder(w).Encode(Result{
			Status:    StatusOK,
			Stdout:    "hi",
			Artifacts: []Artifact{{Kind: ArtifactChart, Bytes: []byte(`{"type":"bar"}`)}},
			Usage:     Usage{WallMS: 5},
		})
	}))
	defer srv.Close()

	r := NewHTTPRunner(srv.URL, "secret-token")
	res, err := r.Run(context.Background(), Job{ID: "j1", Language: LanguagePython, Code: "print('hi')", Limits: DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if gotToken != "secret-token" {
		t.Errorf("token not sent: %q", gotToken)
	}
	if gotJob.ID != "j1" || gotJob.Code != "print('hi')" {
		t.Errorf("job not marshaled correctly: %+v", gotJob)
	}
	if res.Status != StatusOK || res.Stdout != "hi" || len(res.Artifacts) != 1 {
		t.Errorf("result not decoded: %+v", res)
	}
}

func TestHTTPRunner_BusyAndErrorStatuses(t *testing.T) {
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer busy.Close()
	if _, err := NewHTTPRunner(busy.URL, "t").Run(context.Background(), Job{Limits: DefaultLimits()}); err == nil {
		t.Error("503 should return an error (=> unavailable)")
	}

	boom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer boom.Close()
	if _, err := NewHTTPRunner(boom.URL, "t").Run(context.Background(), Job{Limits: DefaultLimits()}); err == nil {
		t.Error("500 should return an error")
	}
}
