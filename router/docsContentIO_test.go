package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// A workspace that turns docs.export.members off: a member's export, and the
// record of an export the browser made, answer 403; an admin's go through.

type exportOff struct{}

func (exportOff) Bool(_ context.Context, p policyBusiness.BoolPolicy) bool { return false }
func (exportOff) Int(_ context.Context, p policyBusiness.IntPolicy) int    { return p.Default }
func (exportOff) List(_ context.Context, p policyBusiness.ListPolicy) []string {
	return p.Default
}
func (exportOff) Require(policyBusiness.BoolPolicy) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if u, ok := r.Context().Value(helpers.UserInfoContextKey).(userModels.UserInfo); ok && u.UserPostgresInfo.IsAdmin {
				next.ServeHTTP(w, r)
				return
			}
			w.WriteHeader(http.StatusForbidden)
		})
	}
}

// recording is a pipeline that only says it was asked.
type recording struct {
	contentIOBusiness.Service
	asked []string
}

func (r *recording) RecordExport(_ context.Context, _ accessBusiness.Subject, res accessBusiness.Resource, format string) error {
	r.asked = append(r.asked, "record "+format)
	return nil
}

func (r *recording) Export(_ context.Context, _ accessBusiness.Subject, _ accessBusiness.Resource, format string, to contentIOBusiness.ExportTarget) error {
	r.asked = append(r.asked, "export "+format)
	_, _ = io.WriteString(to.Start("x.md", "text/markdown"), "# x\n")
	return nil
}

func TestExportsFollowTheWorkspacePolicy(t *testing.T) {
	savedPolicies, savedPipeline := policyBusiness.Policies, contentIOBusiness.Pipeline
	t.Cleanup(func() { policyBusiness.Policies, contentIOBusiness.Pipeline = savedPolicies, savedPipeline })
	policyBusiness.Policies = exportOff{}
	pipe := &recording{}
	contentIOBusiness.Pipeline = pipe

	doc := chi.NewRouter()
	mountContentIO(docsRouters{Doc: doc, Member: chi.NewRouter(), Admin: chi.NewRouter(), Internal: chi.NewRouter()})

	as := func(admin bool) context.Context {
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = uuid.New()
		u.UserPostgresInfo.IsAdmin = admin
		u.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: "0x1"}
		return context.WithValue(context.Background(), helpers.UserInfoContextKey, u)
	}
	body := `{"doc_uuid":"33333333-3333-3333-3333-333333333333","format":"md"}`
	for _, c := range []struct {
		path  string
		admin bool
		want  int
	}{
		{"/export", false, http.StatusForbidden},
		{"/recordExport", false, http.StatusForbidden},
		{"/export", true, http.StatusOK},
		{"/recordExport", true, http.StatusNoContent},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(body)).WithContext(as(c.admin))
		doc.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("POST %s (admin %v) answered %d, want %d", c.path, c.admin, rec.Code, c.want)
		}
	}
	if strings.Join(pipe.asked, ",") != "export md,record md" {
		t.Errorf("the pipeline was asked %v; with the policy off a member's request must not reach it", pipe.asked)
	}
}
