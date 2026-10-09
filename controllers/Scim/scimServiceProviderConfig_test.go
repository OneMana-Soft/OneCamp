package controllers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Okta and Entra read ServiceProviderConfig while a connection is set up and
// show its documentation link to the admin doing it. It pointed at
// onecamp.in, a domain OneCamp doesn't use.
func TestSCIMDocumentationLinkIsOneCampsDocs(t *testing.T) {
	rec := httptest.NewRecorder()
	GetScimServiceProviderConfig(rec, httptest.NewRequest(http.MethodGet, "/scim/v2/ServiceProviderConfig", nil))
	var body struct {
		DocumentationURI string `json:"documentationUri"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ServiceProviderConfig answered %d %s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(body.DocumentationURI, "https://onemana.dev/docs") {
		t.Fatalf("documentationUri is %q, want OneCamp's docs at https://onemana.dev/docs", body.DocumentationURI)
	}
}
