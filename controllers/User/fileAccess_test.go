package controllers

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// docRules decides a doc for each person by their account id; anyone else
// isn't told it exists. It records who it was asked about.
type docRules struct {
	accessBusiness.Resolver
	levels map[string]accessBusiness.Decision
	asked  *[]accessBusiness.Subject
}

func (d docRules) Resolve(_ context.Context, who accessBusiness.Subject, _ accessBusiness.Resource) (accessBusiness.Decision, error) {
	*d.asked = append(*d.asked, who)
	got, ok := d.levels[who.UserUUID]
	if !ok {
		return accessBusiness.Decision{}, accessBusiness.ErrNotFound
	}
	return got, nil
}

// uploadToDoc posts a file for a doc as /uploadFile's sign-in leaves the
// request: the person's Postgres account, and no graph identity.
func uploadToDoc(t *testing.T, person uuid.UUID, docUUID string) *httptest.ResponseRecorder {
	t.Helper()
	return uploadAs(t, person, `{"src_key":"doc","src_value":"`+docUUID+`"}`)
}

// uploadAs posts a file with jsonData as /uploadFile's sign-in leaves the
// request.
func uploadAs(t *testing.T, person uuid.UUID, jsonData string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "notes.txt")
	_, _ = part.Write([]byte("hello"))
	_ = mw.WriteField("jsonData", jsonData)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/uploadFile", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var u models.UserInfo
	u.UserPostgresInfo.Id = person
	req = req.WithContext(context.WithValue(req.Context(), helpers.UserInfoContextKey, u))
	rec := httptest.NewRecorder()
	UploadFile(rec, req)
	return rec
}

// A file for a doc needs the access a comment does. Someone who may only view
// it is refused, and someone it isn't shared with, or who never saw it before
// it was deleted, gets the 404 a doc that isn't there gets. Nothing is stored
// for any of them (the store isn't set up here, so reaching it would fail
// with another code).
func TestOnlySomeoneWhoMayAddToADocUploadsAFileForIt(t *testing.T) {
	viewer, stranger, formerViewer := uuid.New(), uuid.New(), uuid.New()
	var asked []accessBusiness.Subject
	saved := accessBusiness.Rules
	defer func() { accessBusiness.Rules = saved }()
	accessBusiness.Rules = docRules{asked: &asked, levels: map[string]accessBusiness.Decision{
		viewer.String():       {Level: accessBusiness.LevelView, Was: accessBusiness.LevelView},
		formerViewer.String(): {Deleted: true, Level: accessBusiness.LevelNone, Was: accessBusiness.LevelNone},
	}}
	doc := uuid.NewString()

	if rec := uploadToDoc(t, viewer, doc); rec.Code != http.StatusForbidden {
		t.Fatalf("a viewer: %d %s", rec.Code, rec.Body)
	}
	for name, who := range map[string]uuid.UUID{"someone it isn't shared with": stranger, "someone who couldn't see it before it was deleted": formerViewer} {
		rec := uploadToDoc(t, who, doc)
		if rec.Code != http.StatusNotFound || bytes.Contains(rec.Body.Bytes(), []byte(doc)) {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := uploadToDoc(t, viewer, "not-a-doc"); rec.Code != http.StatusNotFound {
		t.Fatalf("a doc id that isn't one: %d %s", rec.Code, rec.Body)
	}
	// The sign-in gives no graph uid, so the person is asked about by account.
	if len(asked) == 0 || asked[0].UserUUID != viewer.String() {
		t.Fatalf("the resolver was asked about %+v", asked)
	}
}

// A file goes only where the uploader may write. A kind with no rule (an
// import's task or comment), no kind, and a place named so that it can't be
// theirs, are refused in the uploader's words before anything is stored (the
// store isn't set up here, so reaching it would fail with another code).
func TestAnUploadToAPlaceWithNoRuleOrNotTheirsIsRefused(t *testing.T) {
	me, ann, ben := uuid.New(), uuid.NewString(), uuid.NewString()
	others := helpers.GetGroupingId(ann, ben)
	for name, jsonData := range map[string]string{
		"an import's task":                 `{"src_key":"task","src_value":"` + uuid.NewString() + `"}`,
		"an import's comment":              `{"src_key":"comment","src_value":"` + uuid.NewString() + `"}`,
		"no kind":                          `{"src_value":"` + uuid.NewString() + `"}`,
		"other people's conversation":      `{"src_key":"chat","src_value":"` + others + `"}`,
		"a conversation id that isn't one": `{"src_key":"chat","src_value":"` + me.String() + `"}`,
		"a group id that isn't one":        `{"src_key":"grpChat","src_value":"not-a-group"}`,
		"several places, one of them other people's conversation": `{"src_key":"chat","chat_uuids":["` + others + `"]}`,
	} {
		rec := uploadAs(t, me, jsonData)
		if rec.Code != http.StatusForbidden || !bytes.Contains(rec.Body.Bytes(), []byte("You can't add files here.")) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

// An upload for several places is asked about each place as what it is: a
// group named among its conversations is a group, whose files are its
// people's, as the rows the upload writes say (models ConversationFileKind).
func TestAnUploadsConversationsAreAskedAboutAsWhatTheyAre(t *testing.T) {
	ann, ben, cat, channel := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	dm := helpers.GetGroupingId(ann, ben)
	group, err := helpers.GenerateGroupID([]string{ann, ben, cat})
	if err != nil {
		t.Fatal(err)
	}
	got := placesSentTo([]string{channel}, []string{dm, group, dm})
	want := []fileAccessBusiness.Place{{Kind: "channel", ID: channel}, {Kind: "chat", ID: dm}, {Kind: "grpChat", ID: group}}
	if !slices.EqualFunc(got, want, func(a, b fileAccessBusiness.Place) bool { return a.Kind == b.Kind && a.ID == b.ID }) {
		t.Errorf("the places asked about: %+v; want %+v", got, want)
	}
}
