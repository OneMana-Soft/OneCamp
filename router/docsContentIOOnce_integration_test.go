//go:build integration

package router

// One import per Idempotency-Key (docs W3), on /doc/import as router/
// mounts it, against real Postgres, Redis, MinIO and Dgraph: a file sent again
// under the same key, as a browser sends a form again (its multipart boundary
// new), is answered from the first answer and makes no second job; the key
// with another file is refused; without a key every send imports. The
// sign-in is a stand-in that puts the member on the request, as VerifyAuth
// does.
//
// Run: PKGS=./router/ (go test -tags=integration ./router/ -run TestADocImportUnderOneIdempotencyKeyRunsOnce -v)

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestADocImportUnderOneIdempotencyKeyRunsOnce(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.SetupMinio(t, "content-io-test")
	integration.StubMqttClient(t)

	person := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, 'sam', 'Sam', NOW(), NOW())`, person, person.String()+"@example.test"); err != nil {
		t.Fatal(err)
	}
	dg := integration.SetupDgraph(t)
	uids := dg.Mutate(t, []map[string]any{{"uid": "_:sam", "dgraph.type": "User", "user_uuid": person.String(), "user_name": "sam",
		"user_full_name": "Sam", "user_deleted_at": "0001-01-01T00:00:00Z"}})
	var sam userModels.UserInfo
	sam.UserPostgresInfo.Id = person
	sam.UserDgraphInfo.Uid = uids["sam"]
	sam.UserDgraphInfo.Uuid = person.String()

	signedIn := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, sam)))
		})
	}
	doc := chi.NewRouter()
	doc.Use(signedIn, customMiddleware.BodyLimit(8<<20))
	mountContentIO(docsRouters{Doc: doc, Member: chi.NewRouter().With(signedIn), Admin: chi.NewRouter(), Internal: chi.NewRouter(), V1: chi.NewRouter(), Files: chi.NewRouter()})

	// send posts a file as the web's import dialog does. Each call writes the
	// form afresh, so each has a boundary of its own.
	send := func(key, name, text string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		part, err := form.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write([]byte(text))
		_ = form.Close()
		req := httptest.NewRequest(http.MethodPost, "/import", &body)
		req.Header.Set("Content-Type", form.FormDataContentType())
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		w := httptest.NewRecorder()
		doc.ServeHTTP(w, req)
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w, out
	}
	jobs := func() int {
		t.Helper()
		var n int
		if err := env.PG.QueryRow(`SELECT count(*) FROM content_io_jobs WHERE user_uuid = $1 AND kind = 'import'`, person).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	jobID := func(out map[string]any) any {
		job, _ := out["job"].(map[string]any)
		return job["id"]
	}

	first, a := send("k-1", "plan.md", "# Plan\n\nShip it.\n")
	if first.Code != http.StatusAccepted || jobID(a) == nil {
		t.Fatalf("the first send answered %d: %v", first.Code, a)
	}
	again, b := send("k-1", "plan.md", "# Plan\n\nShip it.\n")
	if again.Code != http.StatusAccepted || again.Header().Get("Idempotent-Replayed") != "true" || jobID(b) != jobID(a) {
		t.Errorf("the same file again under its key answered %d %v (replayed %q), the first %v", again.Code, b, again.Header().Get("Idempotent-Replayed"), a)
	}
	if n := jobs(); n != 1 {
		t.Errorf("one key made %d imports", n)
	}

	other, c := send("k-1", "notes.md", "# Notes\n")
	if other.Code != http.StatusUnprocessableEntity || c["msg"] == nil {
		t.Errorf("the key with another file answered %d %v", other.Code, c)
	}

	// Without a key, every send is an import of its own.
	send("", "plan.md", "# Plan\n\nShip it.\n")
	send("", "plan.md", "# Plan\n\nShip it.\n")
	if n := jobs(); n != 3 {
		t.Errorf("a key and two sends without one made %d imports", n)
	}
}
