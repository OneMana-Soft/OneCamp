//go:build integration

package integration_test

// basecamp-16: a project's links. The tools its people work in elsewhere, as
// cards on the project's home: whoever may see the project reads them; its
// admins add, rename and delete them (softly: a restore brings one back,
// unless its address was added again since); an archived project's are read
// only. This server reads a link's page through safefetch; a page it can't
// read (the .example hosts here never resolve) keeps a plain link named by its
// host. A link's icon and picture are kept with it and served only by its own
// route: no file route serves one, and no one uploads one.
// Run: integ_rel.sh <wt> <log> 'TestBasecamp16'

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	linkController "github.com/akashc777/OneCamp/controllers/ProjectLink"
	restoreController "github.com/akashc777/OneCamp/controllers/Restore"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

type bc16Link struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	Host        string `json:"host"`
	Title       string `json:"title"`
	Description string `json:"description"`
	HasIcon     bool   `json:"has_icon"`
	HasImage    bool   `json:"has_image"`
}

type bc16List struct {
	Links     []bc16Link `json:"links"`
	CanManage bool       `json:"can_manage"`
}

func bc16Do(s *wa2Scale, who, method string, h http.HandlerFunc, link string, body any) (int, string) {
	s.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	return s.call(who, method, "/", h, map[string]string{"project_uuid": s.project, "link_id": link}, raw)
}

func bc16Read(s *wa2Scale, who string) (int, bc16List, string) {
	s.t.Helper()
	code, out := bc16Do(s, who, http.MethodGet, linkController.ListLinks, "", nil)
	var got struct{ Data bc16List }
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			s.t.Fatalf("links as %s: %v in %s", who, err, out)
		}
	}
	return code, got.Data, out
}

func bc16Add(s *wa2Scale, who, address, title string) (int, bc16Link, string) {
	s.t.Helper()
	code, out := bc16Do(s, who, http.MethodPost, linkController.AddLink, "", map[string]string{"url": address, "title": title})
	var got struct{ Data bc16Link }
	_ = json.Unmarshal([]byte(out), &got)
	return code, got.Data, out
}

// bc16Picture asks for a link's picture as who, and answers the whole response
// (its Location is the stored picture's signed address).
func bc16Picture(s *wa2Scale, who, project, link, kind string) *httptest.ResponseRecorder {
	s.t.Helper()
	ctx := context.Background()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, s.people[who])
	if err != nil || pg == nil {
		s.t.Fatalf("load %s: %v", who, err)
	}
	gr, err := userDomain.GetActiveDgraphUserInfoByUUID(ctx, s.people[who].String())
	if err != nil || gr == nil {
		s.t.Fatalf("load %s's profile: %v", who, err)
	}
	rc := chi.NewRouteContext()
	for k, v := range map[string]string{"project_uuid": project, "link_id": link, "kind": kind} {
		rc.URLParams.Add(k, v)
	}
	c := context.WithValue(context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *gr}), chi.RouteCtxKey, rc)
	rec := httptest.NewRecorder()
	customMiddleware.RequestIDMiddleware(http.HandlerFunc(linkController.GetLinkPicture)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(c))
	return rec
}

func TestBasecamp16LinksAreTheProjectsAdminsToChange(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	s := newWA2Scale(t, env, integration.SetupDgraph(t))
	s.person("vic", true)
	s.person("oz", false)

	// None yet: an empty list, never null; adding is the admins'.
	code, list, out := bc16Read(s, "ana")
	// helpers.WriteJSON indents: the body is compacted before "[]" is looked for.
	var flat bytes.Buffer
	_ = json.Compact(&flat, []byte(out))
	if code != http.StatusOK || list.Links == nil || len(list.Links) != 0 || !list.CanManage || !strings.Contains(flat.String(), `"links":[]`) {
		t.Fatalf("an admin's first read: %d %s", code, out)
	}
	if _, list, out = bc16Read(s, "ben"); list.CanManage {
		t.Fatalf("a member may manage the links: %s", out)
	}
	if code, _, out := bc16Read(s, "oz"); code != http.StatusNotFound {
		t.Fatalf("someone outside the project reads its links: %d %s", code, out)
	}
	for who, want := range map[string]int{"ben": http.StatusForbidden, "vic": http.StatusForbidden, "oz": http.StatusNotFound} {
		if code, _, out := bc16Add(s, who, "https://figma.example/file/abc", ""); code != want {
			t.Fatalf("%s adds a link: %d %s, want %d", who, code, out, want)
		}
	}
	for _, bad := range []string{"javascript:alert(1)", "ftp://files.example/a", ""} {
		if code, _, out := bc16Add(s, "ana", bad, ""); code != http.StatusBadRequest || !strings.Contains(out, "msg") {
			t.Fatalf("adding %q: %d %s, want a 400 in words", bad, code, out)
		}
	}

	// A page this server can't read keeps a plain link named by its host,
	// without its trackers; the same address again is refused.
	code, figma, out := bc16Add(s, "ana", "https://www.figma.example/file/abc?utm_source=chat#frame", "")
	if code != http.StatusOK || figma.ID == "" || figma.URL != "https://www.figma.example/file/abc#frame" || figma.Title != "figma.example" || figma.Host != "figma.example" || figma.HasIcon || figma.HasImage {
		t.Fatalf("a link to a page that can't be read: %d %s", code, out)
	}
	if code, _, out := bc16Add(s, "ana", "https://www.figma.example/file/abc?utm_campaign=launch#frame", "Again"); code != http.StatusConflict {
		t.Fatalf("the same address twice: %d %s, want 409", code, out)
	}
	code, drive, out := bc16Add(s, "ana", "https://drive.example/folders/launch", "  Launch   folder ")
	if code != http.StatusOK || drive.Title != "Launch folder" {
		t.Fatalf("a link with the admin's own title: %d %s", code, out)
	}
	code, list, out = bc16Read(s, "vic")
	if code != http.StatusOK || len(list.Links) != 2 || list.Links[0].ID != figma.ID || list.Links[1].ID != drive.ID || list.CanManage {
		t.Fatalf("a viewer's read, in the order they were added: %d %s", code, out)
	}

	// Renamed by the admins only, never to nothing; another project's link isn't there.
	if code, out := bc16Do(s, "ben", http.MethodPost, linkController.EditLink, figma.ID, map[string]string{"title": "Designs"}); code != http.StatusForbidden {
		t.Fatalf("a member renames a link: %d %s", code, out)
	}
	if code, out := bc16Do(s, "ana", http.MethodPost, linkController.EditLink, figma.ID, map[string]string{"title": " \n "}); code != http.StatusBadRequest {
		t.Fatalf("a blank title: %d %s", code, out)
	}
	if code, out := bc16Do(s, "ana", http.MethodPost, linkController.EditLink, uuid.NewString(), map[string]string{"title": "x"}); code != http.StatusNotFound {
		t.Fatalf("a link the project doesn't have: %d %s", code, out)
	}
	if code, out := bc16Do(s, "ana", http.MethodPost, linkController.EditLink, figma.ID, map[string]string{"title": "Launch designs"}); code != http.StatusOK {
		t.Fatalf("an admin renames a link: %d %s", code, out)
	}
	if _, list, out = bc16Read(s, "ana"); list.Links[0].Title != "Launch designs" {
		t.Fatalf("after the rename: %s", out)
	}

	// Deleted by the admins only, softly: the restore brings it back.
	restore := func(who, id string) (int, string) {
		return s.call(who, http.MethodPost, "/", restoreController.Restore, map[string]string{"kind": "link", "id": id}, nil)
	}
	if code, out := bc16Do(s, "vic", http.MethodPost, linkController.DeleteLink, drive.ID, nil); code != http.StatusForbidden {
		t.Fatalf("a viewer deletes a link: %d %s", code, out)
	}
	if code, out := bc16Do(s, "ana", http.MethodPost, linkController.DeleteLink, drive.ID, nil); code != http.StatusOK {
		t.Fatalf("an admin deletes a link: %d %s", code, out)
	}
	if _, list, out = bc16Read(s, "ana"); len(list.Links) != 1 {
		t.Fatalf("after the delete: %s", out)
	}
	if code, out := bc16Do(s, "ana", http.MethodPost, linkController.DeleteLink, drive.ID, nil); code != http.StatusNotFound {
		t.Fatalf("a link deleted already: %d %s", code, out)
	}
	if code, out := restore("ben", drive.ID); code != http.StatusForbidden {
		t.Fatalf("a member restores a link: %d %s", code, out)
	}
	if code, out := restore("ana", drive.ID); code != http.StatusOK {
		t.Fatalf("an admin restores a link: %d %s", code, out)
	}
	if _, list, out = bc16Read(s, "ana"); len(list.Links) != 2 || list.Links[1].Title != "Launch folder" {
		t.Fatalf("after the restore: %s", out)
	}
	// Deleted again and its address added anew: the old one can't come back over it.
	if code, out := bc16Do(s, "ana", http.MethodPost, linkController.DeleteLink, drive.ID, nil); code != http.StatusOK {
		t.Fatalf("the second delete: %d %s", code, out)
	}
	code, again, out := bc16Add(s, "ana", "https://drive.example/folders/launch", "")
	if code != http.StatusOK || again.ID == drive.ID {
		t.Fatalf("its address added anew: %d %s", code, out)
	}
	if code, out := restore("ana", drive.ID); code != http.StatusConflict {
		t.Fatalf("a restore over a live link to its address: %d %s, want 409", code, out)
	}

	// A project holds at most 50, and a deleted link's restore is held to that
	// too (V-168): at the most it is refused in the add's words and stays
	// deleted; with room it comes back.
	code, wiki, out := bc16Add(s, "ana", "https://wiki.example/launch", "Launch wiki")
	if code != http.StatusOK {
		t.Fatalf("a link to delete before the project is full: %d %s", code, out)
	}
	if code, out := bc16Do(s, "ana", http.MethodPost, linkController.DeleteLink, wiki.ID, nil); code != http.StatusOK {
		t.Fatalf("deleting it before the project is full: %d %s", code, out)
	}
	s.exec(`INSERT INTO project_links (project_uuid, url, title, position) SELECT $1, 'https://filler.example/' || g, 'Filler', 100 + g FROM generate_series(1, 48) g`, s.project)
	if code, _, out := bc16Add(s, "ana", "https://github.example/acme/app", ""); code != http.StatusConflict {
		t.Fatalf("the 51st link: %d %s, want 409", code, out)
	}
	full := "A project holds at most 50 links. Delete one to add another."
	if code, out := restore("ana", wiki.ID); code != http.StatusConflict || msgFromBody(out) != full {
		t.Fatalf("a restore past the project's 50 links: %d %s, want 409 %q", code, out, full)
	}
	s.exec(`DELETE FROM project_links WHERE project_uuid = $1 AND url LIKE 'https://filler.example/%'`, s.project)
	if code, out := restore("ana", wiki.ID); code != http.StatusOK {
		t.Fatalf("its restore once there is room: %d %s", code, out)
	}
	if code, out := bc16Do(s, "ana", http.MethodPost, linkController.DeleteLink, wiki.ID, nil); code != http.StatusOK {
		t.Fatalf("deleting it again: %d %s", code, out)
	}

	// A link deleted before the archive, which a restore would bring back.
	code, plan, out := bc16Add(s, "ana", "https://notes.example/launch-plan", "Launch plan")
	if code != http.StatusOK {
		t.Fatalf("a link to delete before the archive: %d %s", code, out)
	}
	if code, out := bc16Do(s, "ana", http.MethodPost, linkController.DeleteLink, plan.ID, nil); code != http.StatusOK {
		t.Fatalf("deleting it before the archive: %d %s", code, out)
	}

	// The project archived: its links are read only, and a deleted one stays deleted.
	s.dg.Mutate(t, []map[string]any{{"uid": s.uids["p"], "project_deleted_at": "2026-10-01T00:00:00Z"}})
	if code, _, out := bc16Add(s, "ana", "https://github.example/acme/app", ""); code != http.StatusForbidden {
		t.Fatalf("adding to an archived project: %d %s", code, out)
	}
	if code, out := bc16Do(s, "ana", http.MethodPost, linkController.DeleteLink, again.ID, nil); code != http.StatusForbidden {
		t.Fatalf("deleting from an archived project: %d %s", code, out)
	}
	if code, out := restore("ana", plan.ID); code != http.StatusForbidden {
		t.Fatalf("an admin restores a link of an archived project: %d %s, want 403", code, out)
	}
	if code, list, out := bc16Read(s, "ana"); code != http.StatusOK || list.CanManage || len(list.Links) != 2 {
		t.Fatalf("an archived project's links: %d %s", code, out)
	} else {
		for _, l := range list.Links {
			if l.ID == plan.ID {
				t.Fatalf("a link deleted before the archive came back: %s", out)
			}
		}
	}
}

func TestBasecamp16ALinksPicturesAreServedOnlyByItsLink(t *testing.T) {
	env := integration.ConnectServerPostgres(t)
	integration.StubMqttClient(t)
	store := integration.SetupMinio(t, "bc16-links-test")
	s := newWA2Scale(t, env, integration.SetupDgraph(t))
	s.person("vic", true)
	s.person("oz", false)
	ctx := context.Background()

	code, link, out := bc16Add(s, "ana", "https://figma.example/file/abc", "")
	if code != http.StatusOK {
		t.Fatalf("the link: %d %s", code, out)
	}
	// Its icon, as the server keeps one it read from the page.
	key := "projectLinks/" + uuid.NewString() + "-icon.png"
	if _, err := store.PutObject(ctx, helpers.UserUploadBucket(), attachmentBusiness.ObjectName(key), bytes.NewReader(onePixelPNG), int64(len(onePixelPNG)), minio.PutObjectOptions{ContentType: "image/png"}); err != nil {
		t.Fatalf("store the icon: %v", err)
	}
	var icon uuid.UUID
	if err := env.PG.QueryRowContext(ctx, `INSERT INTO attachments (obj_key, src_key, src_value, created_by) VALUES ($1, 'projectLink', $2, $3) RETURNING id`,
		key, s.project, s.people["ana"]).Scan(&icon); err != nil {
		t.Fatalf("the icon's row: %v", err)
	}
	s.exec(`UPDATE project_links SET icon_object = $1 WHERE id = $2`, icon, link.ID)
	if _, list, out := bc16Read(s, "vic"); len(list.Links) != 1 || !list.Links[0].HasIcon || list.Links[0].HasImage {
		t.Fatalf("the list says which pictures a link has: %s", out)
	}

	// Whoever may see the project gets it: a 302 to the bytes kept.
	for _, who := range []string{"ana", "ben", "vic"} {
		rec := bc16Picture(s, who, s.project, link.ID, "icon")
		where := rec.Header().Get("Location")
		if rec.Code != http.StatusFound || where == "" {
			t.Fatalf("%s asks for the icon: %d %s", who, rec.Code, rec.Body.String())
		}
		res, err := http.Get(where)
		if err != nil {
			t.Fatalf("%s follows the 302: %v", who, err)
		}
		got, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || !bytes.Equal(got, onePixelPNG) {
			t.Fatalf("%s: the 302's address answered %d with %d bytes, want the %d stored", who, res.StatusCode, len(got), len(onePixelPNG))
		}
	}
	// Outside the project nothing; a picture the link hasn't, or another project's link, is not there.
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"someone outside the project": bc16Picture(s, "oz", s.project, link.ID, "icon"),
		"a picture it hasn't":         bc16Picture(s, "vic", s.project, link.ID, "image"),
		"a link the project hasn't":   bc16Picture(s, "vic", s.project, uuid.NewString(), "icon"),
	} {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d %s, want 404", name, rec.Code, rec.Body.String())
		}
	}

	// No file route serves it, even to the project's admin, and no one uploads one.
	if code, out := s.call("ana", http.MethodGet, "/", userController.GetProjectFile, map[string]string{"project_uuid": s.project, "obj_uuid": icon.String()}, nil); code != http.StatusNotFound {
		t.Fatalf("the project's file route serves a link's icon: %d %s", code, out)
	}
	ana, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, s.people["ana"])
	if err != nil || ana == nil {
		t.Fatalf("load ana: %v", err)
	}
	if code, body := uploadAs(t, *ana, "icon.png", onePixelPNG, `{"src_key":"projectLink","src_value":"`+s.project+`"}`); code != http.StatusForbidden {
		t.Fatalf("an upload as a link's picture: %d %v, want 403", code, body)
	}
}
