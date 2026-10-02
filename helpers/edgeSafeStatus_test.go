package helpers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestWriteJSONNeverSendsAStatusTheEdgeReplaces(t *testing.T) {
	for _, in := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		w := httptest.NewRecorder()
		WriteJSON(w, in, Envolope{"msg": "Gmail did not answer"})
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "Gmail did not answer") {
			t.Errorf("%d became %d with body %q", in, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	WriteJSON(w, http.StatusConflict, Envolope{})
	if w.Code != http.StatusConflict {
		t.Errorf("an ordinary status changed: %d", w.Code)
	}
}

// Handlers should say 503 themselves rather than lean on WriteJSON's net:
// http.Error and w.WriteHeader do not pass through it.
func TestNoHandlerAnswersWithAGatewayStatus(t *testing.T) {
	banned := regexp.MustCompile(`http\.Status(BadGateway|GatewayTimeout)`)
	var hits []string
	for _, root := range []string{"../controllers", "../middleware", "../router"} {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, _ := os.ReadFile(path)
			for i, line := range strings.Split(string(b), "\n") {
				if banned.MatchString(line) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
					hits = append(hits, path+":"+strconv.Itoa(i+1))
				}
			}
			return nil
		})
	}
	if len(hits) > 0 {
		t.Errorf("a 502/504 reaches the browser as a CORS-less edge page (\"Network Error\"); use 503:\n  %s",
			strings.Join(hits, "\n  "))
	}
}
