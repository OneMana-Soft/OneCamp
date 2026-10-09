package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
