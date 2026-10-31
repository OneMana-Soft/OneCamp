package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// Jira refusing the credentials is a refused token wherever it surfaces;
// any other error status isn't.
func TestJiraRefusingTheCredentialsIsARefusedToken(t *testing.T) {
	status := http.StatusUnauthorized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	for code, refused := range map[int]bool{http.StatusUnauthorized: true, http.StatusForbidden: true, http.StatusNotFound: false, http.StatusBadRequest: false} {
		status = code
		_, err := New().listProjects(context.Background(), "tok", srv.URL)
		if err == nil || errors.Is(err, importProvider.ErrTokenRejected) != refused {
			t.Errorf("HTTP %d: %v", code, err)
		}
	}
}

// What Jira says with an error is kept to 2 KB, whatever it sends.
func TestJirasErrorBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, strings.Repeat("<html>", 100<<10))
	}))
	defer srv.Close()
	_, err := New().listProjects(context.Background(), "tok", srv.URL)
	if err == nil || len(err.Error()) > 2<<10+100 {
		t.Fatalf("a 502 with a 600 KB page is an error of %d bytes", len(fmt.Sprint(err)))
	}
}
