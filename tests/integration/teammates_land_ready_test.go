//go:build integration
// +build integration

package integration_test

// Teammates land ready.
//
// A person joining a workspace used to arrive on an empty Home in no channel,
// and joined #general by hand, if they knew to. Every way in now puts them in
// the workspace's default channels (#general until an admin chooses) through
// the ordinary membership path, and the sign-in answers with the channel to
// open on. These run the join paths against real Postgres, Redis and Dgraph.
//
// Run: go test -tags=integration ./tests/integration/ -run TestTeammates -v

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"golang.org/x/crypto/bcrypt"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	onboardingBusiness "github.com/akashc777/OneCamp/business/Onboarding"
	scimBusiness "github.com/akashc777/OneCamp/business/Scim"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	authController "github.com/akashc777/OneCamp/controllers/Auth"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	settingsController "github.com/akashc777/OneCamp/controllers/Settings"
	userController "github.com/akashc777/OneCamp/controllers/User"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	emailService "github.com/akashc777/OneCamp/services/Email"
	ldapService "github.com/akashc777/OneCamp/services/LDAP"
	"github.com/akashc777/OneCamp/tests/integration"
)

// landReady is one workspace for these tests: an owner, and the helpers that
// ask the stores what a person ended up with.
type landReady struct {
	t     *testing.T
	ctx   context.Context
	env   *integration.Env
	dg    *integration.DgraphEnv
	owner userModels.UserInfo
}

func setupLandReady(t *testing.T) *landReady {
	t.Helper()
	t.Setenv("JWT_SECRET", "teammates-land-ready-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	t.Setenv("FRONTEND_DOMAIN", "https://team.example.test")
	env := integration.SetupEnv(t)
	ctx := context.Background()
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatalf("wire the project pool at the test database: %v", err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	// People and channels are indexed in the background; give them somewhere to go.
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client
	oldLimit := helpers.SeatLimit
	t.Cleanup(func() { helpers.SeatLimit = oldLimit })
	helpers.SeatLimit = ""

	// A fresh database, so nothing a previous test chose (default channels) applies.
	settingsBusiness.ForgetSettingsForTest()
	t.Cleanup(settingsBusiness.ForgetSettingsForTest)
	lr := &landReady{t: t, ctx: ctx, env: env, dg: dg}
	ownerID, _, err := userBusiness.CreateUserWithMethod(ctx, "owner@example.test", "Owner", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatalf("make the owner: %v", err)
	}
	// The workspace's admin, as the first account is.
	if err := userBusiness.CreateAdminUser(ctx, "owner@example.test"); err != nil {
		t.Fatalf("make the owner an admin: %v", err)
	}
	lr.owner = lr.info(ownerID)
	return lr
}

// info is what the middleware would hand a handler for this person.
func (lr *landReady) info(id uuid.UUID) userModels.UserInfo {
	lr.t.Helper()
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(lr.ctx, id)
	if err != nil || pg == nil {
		lr.t.Fatalf("load %s: %v", id, err)
	}
	node, err := userDomain.GetDgraphUserInfoByUUID(lr.ctx, id.String())
	if err != nil || node == nil {
		lr.t.Fatalf("load %s's node: %v", id, err)
	}
	return userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *node}
}

// channel makes a channel as the owner, the way the product does.
func (lr *landReady) channel(name string, private bool) uuid.UUID {
	lr.t.Helper()
	err, id := channelBusiness.CreateChannel(lr.ctx, &channelAdapter.InputCreateChannel{ChannelName: name, ChannelPrivate: private}, &lr.owner)
	if err != nil {
		lr.t.Fatalf("create #%s: %v", name, err)
	}
	return id
}

// archive archives a channel the way its admin does.
func (lr *landReady) archive(id uuid.UUID, name string) {
	lr.t.Helper()
	if err := channelBusiness.UpdateChannelInfo(lr.ctx, &channelAdapter.UpdateChannelInfo{
		ChannelUuid: id.String(), ChannelName: name, ChannelArchived: true,
	}, id); err != nil {
		lr.t.Fatalf("archive #%s: %v", name, err)
	}
}

// userID is the account an address has.
func (lr *landReady) userID(email string) uuid.UUID {
	lr.t.Helper()
	var id uuid.UUID
	if err := lr.env.PG.QueryRow(`SELECT id FROM users WHERE email_id = $1`, email).Scan(&id); err != nil {
		lr.t.Fatalf("no account for %s: %v", email, err)
	}
	return id
}

// memberOf asks the graph, which is what every channel list reads.
func (lr *landReady) memberOf(userID, channelID uuid.UUID) bool {
	lr.t.Helper()
	node, err := userDomain.GetDgraphUserInfoByUUID(lr.ctx, userID.String())
	if err != nil || node == nil {
		lr.t.Fatalf("load %s's node: %v", userID, err)
	}
	info, err := channelBusiness.GetBasicDgraphChannelInfoByUUID(lr.ctx, channelID, node.Uid)
	if err != nil {
		lr.t.Fatalf("read channel %s: %v", channelID, err)
	}
	return info != nil && info.IsMember > 0
}

// joinedProperly reports whether joining set up what opening the channel
// needs: the last-seen marker (unread counts) and, a moment later, the
// notification preference row.
func (lr *landReady) joinedProperly(userID, channelID uuid.UUID) bool {
	lr.t.Helper()
	var seen bool
	if err := lr.env.PG.QueryRow(`SELECT EXISTS (SELECT 1 FROM last_seen_channel WHERE user_id = $1 AND channel_id = $2)`,
		userID, channelID).Scan(&seen); err != nil {
		lr.t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var notified bool
		if err := lr.env.PG.QueryRow(`SELECT EXISTS (SELECT 1 FROM users_channel_notification WHERE user_id = $1 AND channel_id = $2)`,
			userID, channelID).Scan(&notified); err != nil {
			lr.t.Fatal(err)
		}
		if notified || time.Now().After(deadline) {
			return seen && notified
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (lr *landReady) invite(email, token string) {
	lr.t.Helper()
	if err := userDomain.AddInvitationWithToken(lr.ctx, email, lr.owner.UserPostgresInfo.Id, token, time.Now().Add(time.Hour)); err != nil {
		lr.t.Fatal(err)
	}
}

func postJSON(t *testing.T, h http.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)))
	return rec
}

func landingIn(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var answer struct {
		Landing string `json:"landing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("answer is not JSON: %s", rec.Body.String())
	}
	return answer.Landing
}

func TestTeammatesLandInTheDefaultChannels(t *testing.T) {
	lr := setupLandReady(t)
	ctx := lr.ctx
	general := lr.channel("general", false)
	news := lr.channel("news", false)
	secret := lr.channel("secret", true)
	old := lr.channel("old", false)
	lr.archive(old, "old")
	generalLanding := "/app/channel/" + general.String() + "?compose=1"

	// 1. Until an admin chooses, everyone who joins is put in #general, by the
	// ordinary membership path, and the sign-in says to open it.
	lr.invite("ana@example.test", "tok-ana")
	rec := postJSON(t, authController.EmailSignup, map[string]string{"token": "tok-ana", "username": "ana", "password": "a long enough password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("accepting the invitation: %d %s", rec.Code, rec.Body.String())
	}
	if got := landingIn(t, rec); got != generalLanding {
		t.Errorf("signing up lands on %q, want %q", got, generalLanding)
	}
	ana := lr.userID("ana@example.test")
	if !lr.memberOf(ana, general) {
		t.Fatal("a new member is not in #general")
	}
	if !lr.joinedProperly(ana, general) {
		t.Error("joining #general skipped the last-seen or notification rows the membership path sets up")
	}
	if lr.memberOf(ana, news) || lr.memberOf(ana, secret) {
		t.Error("a new member was put in channels nobody chose for them")
	}

	// 2. Google: an invited person joins and lands in #general; a member signing
	// in again keeps Home.
	lr.invite("ben@example.test", "tok-ben")
	joined, err := userBusiness.AdmitOAuthUser(ctx, "ben@example.test", "Ben", "google", "")
	if err != nil {
		t.Fatalf("an invited person joining through Google: %v", err)
	}
	if joined.Landing != general || !lr.memberOf(lr.userID("ben@example.test"), general) {
		t.Errorf("joining through Google: landing %v, in #general %v", joined.Landing, lr.memberOf(lr.userID("ben@example.test"), general))
	}
	if again, err := userBusiness.AdmitOAuthUser(ctx, "ben@example.test", "Ben", "google", ""); err != nil || again.Landing != uuid.Nil {
		t.Errorf("a member signing in again: landing %v (%v), want Home", again.Landing, err)
	}

	// 3. An admin chooses: private and archived channels are refused, a public
	// live one is taken, and the choice is what new members join.
	for name, id := range map[string]uuid.UUID{"private": secret, "archived": old} {
		if _, err := channelBusiness.SetDefaultChannels(ctx, []string{general.String(), id.String()}); !errors.Is(err, channelBusiness.ErrNotJoinable) {
			t.Errorf("choosing a %s channel for new members: %v, want refused", name, err)
		}
	}
	if _, err := channelBusiness.SetDefaultChannels(ctx, []string{"general"}); !errors.Is(err, channelBusiness.ErrNotAChannel) {
		t.Errorf("choosing something that is not a channel id: %v, want refused", err)
	}
	if _, err := channelBusiness.SetDefaultChannels(ctx, []string{news.String(), general.String()}); err != nil {
		t.Fatalf("choosing #news and #general: %v", err)
	}
	if chosen, isChoice, err := channelBusiness.DefaultChannels(ctx); err != nil || !isChoice || len(chosen) != 2 || chosen[0].UUID != news {
		t.Fatalf("the choice reads back as %v chosen=%v (%v)", chosen, isChoice, err)
	}

	// 4. Directory sign-in (LDAP) makes the member, puts them in both, and
	// lands them in #general, the workspace's hello, though #news is first.
	t.Setenv("LDAP_ENABLED", "true")
	t.Setenv("LDAP_HOST", "directory.example.test")
	t.Setenv("LDAP_BASE_DN", "dc=example,dc=test")
	t.Setenv("LDAP_USER_FILTER", "(uid=%s)")
	t.Cleanup(authController.UseDirectoryForTest(func(_ *ldapService.LDAPClient, who, _ string) (*ldapService.LDAPUser, error) {
		return &ldapService.LDAPUser{DN: "uid=" + who, Email: who + "@example.test", Username: who}, nil
	}))
	rec = postJSON(t, authController.LDAPLogin, map[string]string{"username_or_email": "cleo", "password": "directory password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("directory sign-in: %d %s", rec.Code, rec.Body.String())
	}
	cleo := lr.userID("cleo@example.test")
	if got := landingIn(t, rec); got != generalLanding || !lr.memberOf(cleo, news) || !lr.memberOf(cleo, general) {
		t.Errorf("directory sign-in: landing %q, in #news %v, in #general %v", got, lr.memberOf(cleo, news), lr.memberOf(cleo, general))
	}
	rec = postJSON(t, authController.LDAPLogin, map[string]string{"username_or_email": "cleo", "password": "directory password"})
	if got := landingIn(t, rec); rec.Code != http.StatusOK || got != "" {
		t.Errorf("signing in through the directory again lands on %q (%d), want Home", got, rec.Code)
	}

	// 5. A channel archived after it was chosen is skipped, and the join goes on.
	lr.archive(news, "news")
	lr.invite("dev@example.test", "tok-dev")
	rec = postJSON(t, authController.EmailSignup, map[string]string{"token": "tok-dev", "username": "dev", "password": "a long enough password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("joining while a chosen channel is archived: %d %s", rec.Code, rec.Body.String())
	}
	if dev := lr.userID("dev@example.test"); lr.memberOf(dev, news) || !lr.memberOf(dev, general) {
		t.Errorf("an archived default channel took a member (%v), or #general did not (%v)", lr.memberOf(dev, news), lr.memberOf(dev, general))
	}

	// 6. The directory provisions an account (SCIM): it is put in the default
	// channels, and its owner's first sign-in lands there.
	created, err := scimBusiness.CreateUser(ctx, scimBusiness.ScimUserResource{UserName: "erin@example.test", DisplayName: "Erin"}, "https://team.example.test/scim/v2")
	if err != nil {
		t.Fatalf("SCIM create: %v", err)
	}
	erin := uuid.MustParse(created.Id)
	if !lr.memberOf(erin, general) {
		t.Error("an account the directory made is not in #general")
	}
	if got := channelBusiness.FirstSignInLanding(ctx, erin); got != general {
		t.Errorf("the provisioned person's first sign-in lands on %v, want #general", got)
	}
	_ = userDomain.RecordLoginMethod(ctx, erin, userModels.AuthMethodSAML)
	if got := channelBusiness.FirstSignInLanding(ctx, erin); got != uuid.Nil {
		t.Errorf("a later sign-in lands on %v, want Home", got)
	}
	if got := channelBusiness.FirstSignInLanding(ctx, ana); got != uuid.Nil {
		t.Errorf("a member who signed up lands on %v at a later sign-in, want Home", got)
	}

	// 7. Many people joining at once each get in: every join writes the same
	// channel node, and the losers of each round are retried, not dropped.
	const crowd = 10
	var wg sync.WaitGroup
	results := make(chan error, crowd)
	for i := 0; i < crowd; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := userBusiness.JoinAsMember(ctx, fmt.Sprintf("crowd%d@example.test", i), fmt.Sprintf("crowd%d", i), nil, "google", false)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("a join in the crowd failed: %v", err)
		}
	}
	missing := []string{}
	for i := 0; i < crowd; i++ {
		email := fmt.Sprintf("crowd%d@example.test", i)
		if !lr.memberOf(lr.userID(email), general) {
			missing = append(missing, email)
		}
	}
	if len(missing) > 0 {
		t.Errorf("joining at the same moment left %d of %d out of #general: %s", len(missing), crowd, strings.Join(missing, ", "))
	}

	// 8. An admin may choose none: then nobody is put anywhere, and new members
	// start on Home.
	if _, err := channelBusiness.SetDefaultChannels(ctx, []string{}); err != nil {
		t.Fatalf("choosing no channel: %v", err)
	}
	joined, err = userBusiness.JoinAsMember(ctx, "fay@example.test", "fay", nil, "google", false)
	if err != nil || joined.Landing != uuid.Nil || lr.memberOf(joined.UserID, general) {
		t.Errorf("with no default channels: landing %v, in #general %v (%v)", joined.Landing, lr.memberOf(joined.UserID, general), err)
	}
}

// One person, one account, whatever case their address arrives in.
//
// An import or a GitHub sync leaves an external row for someone it knows, so
// what it brought in has an author. The directory provisioning that person
// (SCIM) used to create an account beside it and fail on the unique address
// with a 500 the IdP could do nothing about; it now adopts the row, as every
// other way in does. And an address is one mailbox in any case: the person
// invited as ana@ who signs in as Ana@ reaches the same account, and an account
// made before addresses were lowercased still signs in.
func TestTeammatesAreOneAccountWhateverTheCase(t *testing.T) {
	lr := setupLandReady(t)
	ctx := lr.ctx
	general := lr.channel("general", false)

	// An external row, as an import leaves it, in the case the import used.
	priya := uuid.New()
	if _, err := lr.env.PG.Exec(`INSERT INTO users (id, email_id, username, display_name, is_external, created_at, updated_at)
		VALUES ($1, 'Priya.Raman@Acme.test', 'slack-priya', 'Priya Raman', true, NOW(), NOW())`, priya); err != nil {
		t.Fatal(err)
	}
	lr.dg.Mutate(t, map[string]any{"uid": "_:p", "dgraph.type": "User", "user_uuid": priya.String(),
		"user_email_id": "Priya.Raman@Acme.test", "user_name": "Priya Raman", "user_full_name": "Priya Raman", "is_external": true})

	// 1. SCIM adopts it: the same id, a member in both stores, in #general.
	created, err := scimBusiness.CreateUser(ctx, scimBusiness.ScimUserResource{UserName: "priya.raman@acme.test", DisplayName: "Priya Raman"},
		"https://team.example.test/scim/v2")
	if err != nil {
		t.Fatalf("provisioning someone an import already knows: %v", err)
	}
	if created.Id != priya.String() {
		t.Fatalf("SCIM made account %s beside the imported row %s instead of adopting it", created.Id, priya)
	}
	var external bool
	var method string
	if err := lr.env.PG.QueryRow(`SELECT is_external, signup_method FROM users WHERE id = $1`, priya).Scan(&external, &method); err != nil {
		t.Fatal(err)
	}
	node, err := userDomain.GetDgraphUserInfoByUUID(ctx, priya.String())
	if err != nil || node == nil {
		t.Fatalf("load the adopted node: %v", err)
	}
	if external || node.IsExternal || method != userModels.AuthMethodSCIM {
		t.Errorf("after adoption: external %v (graph %v), signed up by %q, want a SCIM member", external, node.IsExternal, method)
	}
	if !lr.memberOf(priya, general) {
		t.Error("the adopted member is not in #general")
	}
	// Provisioning them again, in any case, is the conflict SCIM expects.
	if _, err := scimBusiness.CreateUser(ctx, scimBusiness.ScimUserResource{UserName: "PRIYA.RAMAN@ACME.TEST"}, "https://team.example.test/scim/v2"); !errors.Is(err, scimBusiness.ErrScimUserExists) {
		t.Errorf("provisioning an existing member again: %v, want a uniqueness conflict", err)
	}

	// 2. A new account keeps its address lowercased, whatever case it came in.
	created, err = scimBusiness.CreateUser(ctx, scimBusiness.ScimUserResource{UserName: " Noor@Acme.TEST "}, "https://team.example.test/scim/v2")
	if err != nil {
		t.Fatalf("provisioning someone new: %v", err)
	}
	var stored string
	if err := lr.env.PG.QueryRow(`SELECT email_id FROM users WHERE id = $1`, created.Id).Scan(&stored); err != nil || stored != "noor@acme.test" {
		t.Errorf("a new account's address is stored as %q (%v), want noor@acme.test", stored, err)
	}

	// 3. Invited at one case, joining through Google at another: admitted, one
	// account, at the lowercased address.
	lr.invite("omar@acme.test", "tok-omar")
	if _, err := userBusiness.AdmitOAuthUser(ctx, "Omar@Acme.test", "Omar", "google", ""); err != nil {
		t.Fatalf("an invited person whose provider capitalises their address: %v", err)
	}
	var accounts int
	if err := lr.env.PG.QueryRow(`SELECT count(*) FROM users WHERE LOWER(email_id) = 'omar@acme.test'`).Scan(&accounts); err != nil || accounts != 1 {
		t.Errorf("omar has %d accounts (%v), want 1", accounts, err)
	}
	if lr.userID("omar@acme.test") == uuid.Nil {
		t.Error("omar's address was not stored lowercased")
	}

	// 4. An account made before addresses were lowercased is still found: by
	// Google, by the password sign-in, and as an admin.
	lee := uuid.New()
	hash, err := bcrypt.GenerateFromPassword([]byte("lee's password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lr.env.PG.Exec(`INSERT INTO users (id, email_id, username, password_hash, created_at, updated_at)
		VALUES ($1, 'Lee@Acme.test', 'lee', $2, NOW(), NOW())`, lee, string(hash)); err != nil {
		t.Fatal(err)
	}
	lr.dg.Mutate(t, map[string]any{"uid": "_:l", "dgraph.type": "User", "user_uuid": lee.String(),
		"user_email_id": "Lee@Acme.test", "user_name": "lee", "user_full_name": "Lee"})
	if joined, err := userBusiness.AdmitOAuthUser(ctx, "lee@acme.test", "Lee", "google", ""); err != nil || joined.UserID != uuid.Nil {
		t.Errorf("Google treated an existing member as new: joined %v, %v", joined.UserID, err)
	}
	_, access, _, _, _ := userBusiness.LoginUserByEmailID(ctx, "lee@acme.test", "", time.Now().Add(time.Minute).Unix(), time.Now().Add(time.Hour).Unix())
	if access == "" {
		t.Error("Google signs in nobody for an account stored as Lee@Acme.test")
	}
	if code := postJSON(t, authController.EmailLogin, map[string]string{"email": "LEE@acme.test", "password": "lee's password"}).Code; code != http.StatusOK {
		t.Errorf("the password sign-in at another case: %d, want 200", code)
	}
	if err := userBusiness.CreateAdminUser(ctx, "lee@ACME.test", lee.String()); err != nil {
		t.Fatal(err)
	}
	if isAdmin, _, err := userDomain.GetUserSignInFlags(ctx, lee); err != nil || !isAdmin {
		t.Errorf("making lee an admin at another case left him a member (%v)", err)
	}
}

// An invitation says where it stands, and only a live one lets anyone in.
//
// It used to read "Sent" until someone opened its dead link, and forever when
// its person joined through Google or single sign-on (only the password
// sign-up marked it); an expired one still admitted its address through
// Google; and inviting a member emailed them an invitation to a workspace they
// were already in.
func TestInvitationsSayWhereTheyStand(t *testing.T) {
	lr := setupLandReady(t)
	ctx := lr.ctx
	asOwner := context.WithValue(ctx, helpers.UserInfoContextKey, lr.owner)
	call := func(h http.HandlerFunc, body any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asOwner))
		var answer map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return rec.Code, answer
	}
	status := func(email string) string {
		t.Helper()
		var s string
		if err := lr.env.PG.QueryRow(`SELECT status FROM invitations WHERE email = $1`, email).Scan(&s); err != nil {
			t.Fatalf("invitation for %s: %v", email, err)
		}
		return s
	}
	expire := func(email string) {
		t.Helper()
		if _, err := lr.env.PG.Exec(`UPDATE invitations SET token_expires_at = NOW() - interval '1 hour' WHERE email = $1`, email); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Joining marks the invitation, whatever the way in: Google, the
	// directory (LDAP), and the directory's provisioning (SCIM).
	for _, email := range []string{"gia@example.test", "dir@example.test", "sky@example.test"} {
		if code, answer := call(userController.AddInvitation, map[string]string{"email": email}); code != http.StatusOK {
			t.Fatalf("inviting %s: %d %v", email, code, answer)
		}
	}
	if _, err := userBusiness.AdmitOAuthUser(ctx, "gia@example.test", "Gia", "google", ""); err != nil {
		t.Fatalf("joining through Google: %v", err)
	}
	t.Setenv("LDAP_ENABLED", "true")
	t.Setenv("LDAP_HOST", "directory.example.test")
	t.Setenv("LDAP_BASE_DN", "dc=example,dc=test")
	t.Setenv("LDAP_USER_FILTER", "(uid=%s)")
	t.Cleanup(authController.UseDirectoryForTest(func(_ *ldapService.LDAPClient, who, _ string) (*ldapService.LDAPUser, error) {
		return &ldapService.LDAPUser{DN: "uid=" + who, Email: who + "@example.test", Username: who}, nil
	}))
	if rec := postJSON(t, authController.LDAPLogin, map[string]string{"username_or_email": "dir", "password": "pw"}); rec.Code != http.StatusOK {
		t.Fatalf("joining through the directory: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := scimBusiness.CreateUser(ctx, scimBusiness.ScimUserResource{UserName: "sky@example.test"}, "https://team.example.test/scim/v2"); err != nil {
		t.Fatalf("joining through SCIM: %v", err)
	}
	for _, email := range []string{"gia@example.test", "dir@example.test", "sky@example.test"} {
		if got := status(email); got != "joined" {
			t.Errorf("%s joined, and the invitation still says %q", email, got)
		}
	}

	// 2. Only a live invitation admits through Google: an expired one says so.
	if code, _ := call(userController.AddInvitation, map[string]string{"email": "late@example.test"}); code != http.StatusOK {
		t.Fatalf("inviting late@: %d", code)
	}
	expire("late@example.test")
	if _, err := userBusiness.AdmitOAuthUser(ctx, "late@example.test", "Late", "google", ""); !errors.Is(err, userBusiness.ErrInvitationExpired) {
		t.Errorf("an expired invitation through Google: %v, want ErrInvitationExpired", err)
	}
	if _, err := userBusiness.AdmitOAuthUser(ctx, "nobody@example.test", "Nobody", "google", ""); !errors.Is(err, userBusiness.ErrNotInvited) {
		t.Errorf("no invitation through Google: %v, want ErrNotInvited", err)
	}

	// 3. The admin's list says Expired once it is, and for a live one how long
	// it has left and the link to copy.
	if code, _ := call(userController.AddInvitation, map[string]string{"email": "live@example.test"}); code != http.StatusOK {
		t.Fatalf("inviting live@: %d", code)
	}
	rec := httptest.NewRecorder()
	userController.GetAllInvitations(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(asOwner))
	var list struct {
		Data []struct {
			Email         string `json:"email"`
			Status        string `json:"status"`
			ExpiresInDays *int   `json:"expires_in_days"`
			InviteLink    string `json:"invite_link"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("the list: %v %s", err, rec.Body.String())
	}
	seen := map[string]bool{}
	for _, inv := range list.Data {
		seen[inv.Email] = true
		switch inv.Email {
		case "late@example.test":
			if inv.Status != "expired" || inv.InviteLink != "" {
				t.Errorf("an expired invitation lists as %q with link %q", inv.Status, inv.InviteLink)
			}
		case "live@example.test":
			if inv.Status != "sent" || inv.ExpiresInDays == nil || *inv.ExpiresInDays != 7 ||
				!strings.HasPrefix(inv.InviteLink, "https://team.example.test/signup?token=") {
				t.Errorf("a live invitation lists as %+v", inv)
			}
		case "gia@example.test":
			if inv.Status != "joined" || inv.InviteLink != "" {
				t.Errorf("a used invitation lists as %q with link %q", inv.Status, inv.InviteLink)
			}
		}
	}
	if !seen["late@example.test"] || !seen["live@example.test"] {
		t.Fatalf("the list is missing invitations: %s", rec.Body.String())
	}

	// 4. Inviting someone already here is refused, in words that say why; a
	// live invitation is not made twice; an expired one is renewed with a new
	// link, so inviting again works.
	for email, want := range map[string]string{
		"gia@example.test":  "already a member",
		"live@example.test": "already invited",
		"GIA@Example.test":  "already a member",
	} {
		if code, answer := call(userController.AddInvitation, map[string]string{"email": email}); code != http.StatusConflict ||
			!strings.Contains(fmt.Sprint(answer["msg"]), want) {
			t.Errorf("inviting %s: %d %v, want 409 saying %q", email, code, answer, want)
		}
	}
	if err := userBusiness.DeactivateUser(ctx, lr.userID("gia@example.test")); err != nil {
		t.Fatal(err)
	}
	if code, answer := call(userController.AddInvitation, map[string]string{"email": "gia@example.test"}); code != http.StatusConflict ||
		!strings.Contains(fmt.Sprint(answer["msg"]), "Reactivate") {
		t.Errorf("inviting a deactivated member: %d %v, want 409 saying to reactivate them", code, answer)
	}
	code, answer := call(userController.AddInvitation, map[string]string{"email": "late@example.test"})
	if code != http.StatusOK || status("late@example.test") != "sent" {
		t.Errorf("inviting someone whose invitation expired: %d %v, status %q", code, answer, status("late@example.test"))
	}
	if _, err := userBusiness.AdmitOAuthUser(ctx, "late@example.test", "Late", "google", ""); err != nil {
		t.Errorf("the renewed invitation does not admit: %v", err)
	}

	// 5. Sending again: refused for no invitation, and for one already used.
	for email, want := range map[string]string{"nobody@example.test": "no invitation", "sky@example.test": "already"} {
		if code, answer := call(userController.ResendInvitation, map[string]string{"email": email}); code != http.StatusConflict ||
			!strings.Contains(fmt.Sprint(answer["msg"]), want) {
			t.Errorf("sending again to %s: %d %v, want 409 saying %q", email, code, answer, want)
		}
	}
}

// Sign-up asks for a name and gives a handle.
//
// It asked for a "username", checked only its length, and wrote it as the
// name everyone sees; a second person with the same one became "sam_1a2b3",
// in that name too. Now someone signs up with their name, in any language,
// sees it as they typed it, and is given an @handle made from it; the second
// Sam is @sam-2 and is still called Sam.
func TestSignUpAsksForANameAndGivesAHandle(t *testing.T) {
	lr := setupLandReady(t)
	ctx := lr.ctx
	names := func(id uuid.UUID) (handle, display, full string) {
		t.Helper()
		var h sql.NullString
		if err := lr.env.PG.QueryRow(`SELECT username FROM users WHERE id = $1`, id).Scan(&h); err != nil {
			t.Fatal(err)
		}
		node, err := userDomain.GetDgraphUserInfoByUUID(ctx, id.String())
		if err != nil || node == nil {
			t.Fatalf("load %s: %v", id, err)
		}
		return h.String, node.UserName, node.UserFullName
	}
	signUp := func(token, email string, body map[string]string) (int, map[string]any) {
		t.Helper()
		lr.invite(email, token)
		body["token"] = token
		rec := postJSON(t, authController.EmailSignup, body)
		var answer map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return rec.Code, answer
	}

	// 1. A name in any language is taken as typed, and the handle made from it.
	code, answer := signUp("tok-jose", "jose@example.test", map[string]string{"name": " José O'Brien ", "password": "a long enough password"})
	if code != http.StatusOK || answer["handle"] != "josé-obrien" || answer["name"] != "José O'Brien" {
		t.Fatalf("signing up as José O'Brien: %d %v", code, answer)
	}
	if h, display, full := names(lr.userID("jose@example.test")); h != "josé-obrien" || display != "José O'Brien" || full != "José O'Brien" {
		t.Errorf("stored as handle %q, name %q / %q", h, display, full)
	}

	// 2. The same name again: the same display name, the next handle.
	_, answer = signUp("tok-jose2", "jose.two@example.test", map[string]string{"name": "José O'Brien", "password": "a long enough password"})
	if answer["handle"] != "josé-obrien-2" {
		t.Errorf("the second José O'Brien's handle: %v, want josé-obrien-2", answer["handle"])
	}
	if _, display, _ := names(lr.userID("jose.two@example.test")); display != "José O'Brien" {
		t.Errorf("the second José O'Brien is called %q: a collision must not rename anyone", display)
	}

	// 3. A name the rule refuses is refused, saying what a name can be; and a
	// page from before "Your name", sending a username, still works.
	if code, answer := signUp("tok-bad", "bad@example.test", map[string]string{"name": "<b>bold</b>", "password": "a long enough password"}); code != http.StatusBadRequest ||
		!strings.Contains(fmt.Sprint(answer["msg"]), "letters, spaces, apostrophes, hyphens and full stops") {
		t.Errorf("a name with markup: %d %v", code, answer)
	}
	if code, answer := signUp("tok-old", "old@example.test", map[string]string{"username": "Old Page", "password": "a long enough password"}); code != http.StatusOK || answer["handle"] != "old-page" {
		t.Errorf("an older sign-up page: %d %v", code, answer)
	}

	// 4. Someone an import already knows: the name it knows them by is
	// suggested, what they type is what they are called, and their history's
	// account becomes theirs with a handle of its own.
	priya := uuid.New()
	if _, err := lr.env.PG.Exec(`INSERT INTO users (id, email_id, display_name, is_external, created_at, updated_at)
		VALUES ($1, 'priya@example.test', 'Priya R', true, NOW(), NOW())`, priya); err != nil {
		t.Fatal(err)
	}
	lr.dg.Mutate(t, map[string]any{"uid": "_:p", "dgraph.type": "User", "user_uuid": priya.String(),
		"user_email_id": "priya@example.test", "user_name": "Priya R", "user_full_name": "Priya R", "is_external": true})
	lr.invite("priya@example.test", "tok-priya")
	rec := httptest.NewRecorder()
	authController.ValidateInvitationToken(rec, httptest.NewRequest(http.MethodGet, "/auth/validate-token?token=tok-priya", nil))
	var valid struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &valid); err != nil || valid.Name != "Priya R" {
		t.Errorf("the invitation does not suggest the name the import knows: %s", rec.Body.String())
	}
	rec = postJSON(t, authController.EmailSignup, map[string]string{"token": "tok-priya", "name": "Priya Raman", "password": "a long enough password"})
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("an imported person signing up: %d %s", rec.Code, rec.Body.String())
	}
	if h, display, _ := names(priya); h != "priya-raman" || display != "Priya Raman" || answer["handle"] != "priya-raman" {
		t.Errorf("the adopted account: handle %q (answered %v), name %q", h, answer["handle"], display)
	}

	// 5. Two people called Sam through Google: both are Sam, @sam and @sam-2.
	for _, email := range []string{"sam.one@example.test", "sam.two@example.test"} {
		lr.invite(email, "tok-"+email)
		if _, err := userBusiness.AdmitOAuthUser(ctx, email, "Sam", "google", ""); err != nil {
			t.Fatalf("%s through Google: %v", email, err)
		}
	}
	h1, d1, _ := names(lr.userID("sam.one@example.test"))
	h2, d2, _ := names(lr.userID("sam.two@example.test"))
	if h1 != "sam" || h2 != "sam-2" || d1 != "Sam" || d2 != "Sam" {
		t.Errorf("two Sams: @%s %q and @%s %q, want @sam Sam and @sam-2 Sam", h1, d1, h2, d2)
	}
}

// One name rule, and a handle of one's own.
//
// The profile editor refused anything outside ASCII letters, digits, hyphens
// and spaces under 25 bytes, in the display name, every time the profile was
// saved: José, O'Brien, priya.raman and "sam_1a2b3" (what a name collision
// produced) could never save even a job title. Names are now checked by the
// person rule only when they change, and the handle by its own rule, only
// when it is sent and changes.
func TestAProfileSavesWhateverMadeItsName(t *testing.T) {
	lr := setupLandReady(t)
	ctx := lr.ctx
	member := func(email, handle, name string) userModels.UserInfo {
		t.Helper()
		id := uuid.New()
		if _, err := lr.env.PG.Exec(`INSERT INTO users (id, email_id, username, created_at, updated_at) VALUES ($1, $2, $3, NOW(), NOW())`,
			id, email, handle); err != nil {
			t.Fatal(err)
		}
		lr.dg.Mutate(t, map[string]any{"uid": "_:m", "dgraph.type": "User", "user_uuid": id.String(),
			"user_email_id": email, "user_name": name, "user_full_name": name})
		return lr.info(id)
	}
	save := func(who userModels.UserInfo, body map[string]any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		userController.UpdateUserProfile(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).
			WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, who)))
		return rec.Code, rec.Body.String()
	}
	stored := func(who userModels.UserInfo) (handle, display, full, title string) {
		t.Helper()
		if err := lr.env.PG.QueryRow(`SELECT username FROM users WHERE id = $1`, who.UserPostgresInfo.Id).Scan(&handle); err != nil {
			t.Fatal(err)
		}
		node, err := userDomain.GetDgraphUserInfoByUUID(ctx, who.UserPostgresInfo.Id.String())
		if err != nil || node == nil {
			t.Fatal(err)
		}
		return handle, node.UserName, node.UserFullName, node.Title
	}

	// 1. A name from before the rule saves the rest of the profile, sent back
	// as it was, as the editor sends it.
	sam := member("sam@example.test", "sam_1a2b3", "sam_1a2b3")
	if code, body := save(sam, map[string]any{"user_name": "sam_1a2b3", "user_full_name": "sam_1a2b3", "user_job_title": "Designer"}); code != http.StatusOK {
		t.Fatalf("saving a job title under a name from before the rule: %d %s", code, body)
	}
	if _, _, _, title := stored(sam); title != "Designer" {
		t.Errorf("job title %q after saving", title)
	}

	// 2. A name in any language saves; one the rule refuses, when it is new,
	// does not, and says why.
	sam = lr.info(sam.UserPostgresInfo.Id)
	if code, body := save(sam, map[string]any{"user_name": "José O'Brien", "user_full_name": "José Álvarez O'Brien"}); code != http.StatusOK {
		t.Fatalf("saving José O'Brien: %d %s", code, body)
	}
	if _, display, full, _ := stored(sam); display != "José O'Brien" || full != "José Álvarez O'Brien" {
		t.Errorf("stored %q / %q", display, full)
	}
	sam = lr.info(sam.UserPostgresInfo.Id)
	if code, body := save(sam, map[string]any{"user_name": "<b>bold</b>"}); code != http.StatusBadRequest || !strings.Contains(body, "Display name can use letters") {
		t.Errorf("a new name with markup: %d %s", code, body)
	}

	// 3. The handle: kept when not sent, changed when sent, refused when it
	// breaks its rule or someone else has it in any case.
	if h, _, _, _ := stored(sam); h != "sam_1a2b3" {
		t.Errorf("a save without a handle changed it to %q", h)
	}
	if code, body := save(sam, map[string]any{"user_handle": "@Jose"}); code != http.StatusOK {
		t.Fatalf("choosing @jose: %d %s", code, body)
	}
	if h, _, _, _ := stored(sam); h != "jose" {
		t.Errorf("handle %q after choosing @Jose", h)
	}
	ana := member("ana@example.test", "ana", "Ana")
	if code, body := save(ana, map[string]any{"user_handle": "JOSE"}); code != http.StatusConflict || !strings.Contains(body, "@jose is taken") {
		t.Errorf("taking someone else's handle: %d %s", code, body)
	}
	if code, body := save(ana, map[string]any{"user_handle": "ana smith"}); code != http.StatusBadRequest || !strings.Contains(body, "A handle can use") {
		t.Errorf("a handle with a space: %d %s", code, body)
	}
	if code, _ := save(ana, map[string]any{"user_handle": "ana", "user_job_title": "Lead"}); code != http.StatusOK {
		t.Errorf("sending the handle unchanged: %d", code)
	}

	// 4. The profile they read carries the handle.
	rec := httptest.NewRecorder()
	userController.GetLoggedInUserProfile(rec, httptest.NewRequest(http.MethodGet, "/", nil).
		WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, lr.info(sam.UserPostgresInfo.Id))))
	var profile struct {
		Data struct {
			Handle string `json:"user_handle"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &profile); err != nil || profile.Data.Handle != "jose" {
		t.Errorf("the profile's handle: %q (%v) %s", profile.Data.Handle, err, rec.Body.String())
	}
}

// roundTripFunc stands in for the email provider.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// An invitation says what happened to its email.
//
// "Invitation sent" used to mean only that a key was set in the environment:
// the send ran in the background and its answer was dropped, and a key saved
// in Admin > Email was never used at all. The admin now hears whether the
// provider took the email or why not, and has the link either way; the email
// names who invited them and to where, and a reply reaches the inviter.
func TestAnInvitationSaysWhetherItWasEmailed(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("SENDER_EMAIL", "")
	t.Setenv("FE_DOMAIN", "")
	t.Setenv("APP_SECRET_KEK", "teammates-land-ready-integration-kek")
	lr := setupLandReady(t)
	asOwner := context.WithValue(lr.ctx, helpers.UserInfoContextKey, lr.owner)
	invite := func(h http.HandlerFunc, email string) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"email": email})
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(asOwner))
		var answer map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("inviting %s: %d %s", email, rec.Code, rec.Body.String())
		}
		return answer
	}
	linkFor := func(email string) string {
		t.Helper()
		var token string
		if err := lr.env.PG.QueryRow(`SELECT token FROM invitations WHERE email = $1`, email).Scan(&token); err != nil {
			t.Fatal(err)
		}
		return "https://team.example.test/signup?token=" + token
	}

	// The provider: what it was sent, and what it answers.
	var (
		mu       sync.Mutex
		sent     []map[string]any
		keys     []string
		status   = http.StatusOK
		response = `{"id":"em_1"}`
	)
	old := emailService.SharedHTTPClient.Transport
	t.Cleanup(func() { emailService.SharedHTTPClient.Transport = old })
	emailService.SharedHTTPClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		defer mu.Unlock()
		sent, keys = append(sent, body), append(keys, r.Header.Get("Authorization"))
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
	})

	// 1. No key anywhere: the invitation is made, not emailed, and the admin
	// is told so, with the link to send themselves.
	answer := invite(userController.AddInvitation, "ana@outlook.com")
	if answer["email_sent"] != false || answer["email_error"] != "email isn't set up on this server" ||
		answer["invite_link"] != linkFor("ana@outlook.com") || len(sent) != 0 {
		t.Errorf("with no key: %v (sent %d)", answer, len(sent))
	}

	// 2. A key saved in Admin is the one that sends. The email comes from the
	// workspace's own address (not the seeded noreply@onemana.dev), names the
	// inviter and the workspace, and a reply goes to the inviter.
	key := "re_saved_in_admin"
	if err := settingsBusiness.Save(settingsBusiness.SaveInput{ResendAPIKey: &key}); err != nil {
		t.Fatal(err)
	}
	answer = invite(userController.AddInvitation, "ben@outlook.com")
	if answer["email_sent"] != true || answer["email_error"] != "" || answer["invite_link"] != linkFor("ben@outlook.com") {
		t.Errorf("with a key saved in Admin: %v", answer)
	}
	if len(sent) != 1 || keys[0] != "Bearer re_saved_in_admin" {
		t.Fatalf("sent %d, with %v", len(sent), keys)
	}
	email := sent[0]
	if email["from"] != "noreply@team.example.test" || email["reply_to"] != "owner@example.test" ||
		email["subject"] != "You're invited to join team.example.test on OneCamp" {
		t.Errorf("from %v, reply to %v, subject %v", email["from"], email["reply_to"], email["subject"])
	}
	if html := fmt.Sprint(email["html"]); !strings.Contains(html, "Owner invited you to join them at team.example.test.") ||
		!strings.Contains(html, linkFor("ben@outlook.com")) {
		t.Errorf("the email does not say who and where, or lacks the link: %s", html)
	}

	// 3. A sender an admin chose is used as chosen; when the provider refuses
	// it, the admin hears the provider's reason, and keeps the link.
	if _, err := lr.env.PG.Exec(`UPDATE system_configs SET value = 'hello@acme.example' WHERE key = 'sender_email'`); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	status, response = http.StatusForbidden, `{"name":"validation_error","message":"The acme.example domain is not verified."}`
	mu.Unlock()
	answer = invite(userController.AddInvitation, "cam@outlook.com")
	if answer["email_sent"] != false || answer["invite_link"] != linkFor("cam@outlook.com") ||
		answer["email_error"] != "the email provider refused it (The acme.example domain is not verified)" {
		t.Errorf("a refused email: %v", answer)
	}
	if len(sent) != 2 || sent[1]["from"] != "hello@acme.example" {
		t.Errorf("the chosen sender was not used: %v", sent)
	}

	// 4. Sending it again says the same of the new email, and the old link stops.
	mu.Lock()
	status, response = http.StatusOK, `{"id":"em_2"}`
	mu.Unlock()
	before := linkFor("cam@outlook.com")
	answer = invite(userController.ResendInvitation, "cam@outlook.com")
	if answer["email_sent"] != true || answer["invite_link"] == before || answer["invite_link"] != linkFor("cam@outlook.com") {
		t.Errorf("sending again: %v", answer)
	}

	// 5. The sign-up page says who invited them, and to where.
	token := strings.TrimPrefix(linkFor("cam@outlook.com"), "https://team.example.test/signup?token=")
	rec := httptest.NewRecorder()
	authController.ValidateInvitationToken(rec, httptest.NewRequest(http.MethodGet, "/auth/validate-token?token="+token, nil))
	var page map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || page["inviter_name"] != "Owner" || page["workspace"] != "team.example.test" {
		t.Errorf("the sign-up page: %s", rec.Body.String())
	}
	if _, err := lr.env.PG.Exec(`UPDATE invitations SET token_expires_at = NOW() - interval '1 hour' WHERE email = 'cam@outlook.com'`); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	authController.ValidateInvitationToken(rec, httptest.NewRequest(http.MethodGet, "/auth/validate-token?token="+token, nil))
	if !strings.Contains(rec.Body.String(), "This invitation has expired. Ask Owner to send it again.") {
		t.Errorf("an expired link: %s", rec.Body.String())
	}
}

// A sign-in that fails says why, and passwords can be turned off.
//
// Every Google or GitHub refusal came back as "unauthorized", which the
// sign-in page read as "ask for an invitation": pressing Cancel at Google, a
// sign-in left open too long, a full workspace. And AUTH_EMAIL_DISABLED was
// read backwards by the sign-in page and by nothing else, so a workspace that
// turned passwords off still took them.
func TestSignInSaysWhyAndPasswordsCanBeOff(t *testing.T) {
	lr := setupLandReady(t)
	ctx := lr.ctx
	hash, err := bcrypt.GenerateFromPassword([]byte("a long enough password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	hashed := string(hash)
	if _, _, err := userBusiness.CreateUserWithMethod(ctx, "member@example.test", "Member", &hashed, userModels.AuthMethodEmail, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := userBusiness.CreateUserWithMethod(ctx, "admin@example.test", "Admin", &hashed, userModels.AuthMethodEmail, false); err != nil {
		t.Fatal(err)
	}
	if err := userBusiness.CreateAdminUser(ctx, "admin@example.test"); err != nil {
		t.Fatal(err)
	}
	signIn := func(email string) (int, string) {
		t.Helper()
		rec := postJSON(t, authController.EmailLogin, map[string]string{"email": email, "password": "a long enough password"})
		return rec.Code, rec.Body.String()
	}
	emailOffered := func() bool {
		t.Helper()
		rec := httptest.NewRecorder()
		authController.GetEnabledProviders(rec, httptest.NewRequest(http.MethodGet, "/auth/providers", nil))
		var answer struct {
			Providers map[string]bool `json:"providers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		return answer.Providers["email"]
	}

	// 1. Passwords on: members sign in with theirs.
	t.Setenv("AUTH_EMAIL_DISABLED", "")
	if code, body := signIn("member@example.test"); code != http.StatusOK || !emailOffered() {
		t.Fatalf("passwords on: %d %s", code, body)
	}

	// 2. Passwords off: not offered, not taken from a member, and no password
	// account is made from an invitation. An admin's still works: the way back
	// in when single sign-on breaks.
	t.Setenv("AUTH_EMAIL_DISABLED", "true")
	if emailOffered() {
		t.Error("passwords are off and the sign-in page is told to offer them")
	}
	if code, body := signIn("member@example.test"); code != http.StatusForbidden || !strings.Contains(body, "passwords_off") {
		t.Errorf("a member's password with passwords off: %d %s", code, body)
	}
	if code, body := signIn("admin@example.test"); code != http.StatusOK {
		t.Errorf("an admin's password with passwords off: %d %s", code, body)
	}
	lr.invite("pat@example.test", "tok-pat")
	rec := postJSON(t, authController.EmailSignup, map[string]string{"token": "tok-pat", "name": "Pat", "password": "a long enough password"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "passwords_off") {
		t.Errorf("signing up with a password with passwords off: %d %s", rec.Code, rec.Body.String())
	}
	var accounts int
	if err := lr.env.PG.QueryRow(`SELECT COUNT(*) FROM users WHERE email_id = 'pat@example.test'`).Scan(&accounts); err != nil || accounts != 0 {
		t.Errorf("an account was made: %d %v", accounts, err)
	}
	t.Setenv("AUTH_EMAIL_DISABLED", "")

	// 3. Google or GitHub: Cancel says cancelled, and a sign-in that outlived
	// its state (or was opened twice) says start again.
	callback := func(query string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		userController.OAuthCallback(rec, httptest.NewRequest(http.MethodGet, "/oauth_callback/google?"+query, nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("callback %s: %d", query, rec.Code)
		}
		return rec.Header().Get("Location")
	}
	if loc := callback("error=access_denied&state=s1"); !strings.Contains(loc, "error=signin_cancelled") {
		t.Errorf("cancelled at the provider: %s", loc)
	}
	if loc := callback("state=never-issued&code=c1"); !strings.Contains(loc, "error=signin_expired") {
		t.Errorf("a state that is gone: %s", loc)
	}
}

// An allow-list entry for a domain admits its people with no invitation each,
// but only through Google, and only accounts that domain's Google Workspace
// manages (the ID token's hd). A personal Google account with an address
// there, or a GitHub account with one verified, still needs an invitation. A
// public email domain can't be saved, and the audit log names what changed.
func TestAnAllowListedDomainJoins(t *testing.T) {
	lr := setupLandReady(t)
	asOwner := context.WithValue(lr.ctx, helpers.UserInfoContextKey, lr.owner)
	save := func(list ...string) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"allowed_users": list})
		rec := httptest.NewRecorder()
		settingsController.UpdateSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/settings", bytes.NewReader(raw)).WithContext(asOwner))
		return rec.Code, rec.Body.String()
	}
	if code, body := save("@outlook.com"); code != http.StatusBadRequest || !strings.Contains(body, "public email domain") {
		t.Errorf("saving @outlook.com: %d %s", code, body)
	}
	if code, body := save("@old.example", "sam@acme.example"); code != http.StatusOK {
		t.Fatalf("saving: %d %s", code, body)
	}
	if code, body := save("@acme.example", "sam@acme.example"); code != http.StatusOK {
		t.Fatalf("saving: %d %s", code, body)
	}

	joined, err := userBusiness.AdmitOAuthUser(lr.ctx, "ana@acme.example", "Ana", "google", "acme.example")
	if err != nil || joined.UserID == uuid.Nil {
		t.Fatalf("a Google Workspace account at the allowed domain: %+v %v", joined, err)
	}
	if lr.userID("ana@acme.example") != joined.UserID {
		t.Error("no account for the address at the allowed domain")
	}
	for _, c := range []struct{ addr, provider, hd string }{
		{"bo@acme.example", "google", ""},                  // a personal Google account
		{"cy@acme.example", "github", ""},                  // GitHub never admits by a domain
		{"di@mail.acme.example", "google", "acme.example"}, // another domain
		{"ed@notacme.example", "google", "notacme.example"},
	} {
		if _, err := userBusiness.AdmitOAuthUser(lr.ctx, c.addr, "Someone", c.provider, c.hd); !errors.Is(err, userBusiness.ErrNotInvited) {
			t.Errorf("%s through %s (hd %q): %v, want ErrNotInvited", c.addr, c.provider, c.hd, err)
		}
	}
	// An address listed as itself gets in through GitHub too.
	if _, err := userBusiness.AdmitOAuthUser(lr.ctx, "sam@acme.example", "Sam", "github", ""); err != nil {
		t.Errorf("a listed address through GitHub: %v", err)
	}

	// The audit log names what the second save added and removed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var metadata string
		var change struct {
			Added   []string `json:"added"`
			Removed []string `json:"removed"`
		}
		err := lr.env.PG.QueryRow(`SELECT COALESCE(metadata::text, '{}') FROM admin_audit_log WHERE action = 'settings.allowed_users'
			ORDER BY created_at DESC LIMIT 1`).Scan(&metadata)
		if err == nil && json.Unmarshal([]byte(metadata), &change) == nil &&
			strings.Join(change.Added, ",") == "@acme.example" && strings.Join(change.Removed, ",") == "@old.example" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the audit log says %q (%v), want the entries added and removed", metadata, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A member in no channel is offered one on Home: where new members land,
// #general until an admin chooses otherwise, and nothing when there is none.
func TestAMemberInNoChannelIsOfferedOne(t *testing.T) {
	lr := setupLandReady(t)
	suggested := func() map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		channelController.GetSuggestedChannel(rec, httptest.NewRequest(http.MethodGet, "/ch/suggested", nil))
		var answer struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("suggested: %d %s", rec.Code, rec.Body.String())
		}
		return answer.Data
	}
	if got := suggested(); got != nil {
		t.Errorf("with no #general, offered %v", got)
	}
	general := lr.channel("general", false)
	if got := suggested(); got["ch_uuid"] != general.String() || got["ch_name"] != "general" {
		t.Errorf("offered %v, want #general", got)
	}
	news := lr.channel("news", false)
	if _, err := channelBusiness.SetDefaultChannels(lr.ctx, []string{news.String()}); err != nil {
		t.Fatal(err)
	}
	if got := suggested(); got["ch_uuid"] != news.String() {
		t.Errorf("an admin chose #news, and Home offers %v", got)
	}
}

// An invitation made before links had an expiry lasts a week from when it
// was made (models.Invitation.LiveAt, the one predicate). Google and GitHub
// let its address in however old it was, and its sign-up link worked.
func TestAnInvitationWithoutAnExpiryLastsAWeek(t *testing.T) {
	lr := setupLandReady(t)
	for _, row := range []struct {
		email, token string
		age          string
	}{{"old@example.test", "tok-old-legacy", "8 days"}, {"recent@example.test", "tok-recent-legacy", "2 days"}} {
		if _, err := lr.env.PG.Exec(`INSERT INTO invitations (email, invited_by, status, token, token_expires_at, created_at)
			VALUES ($1, $2, 'pending', $3, NULL, NOW() - $4::interval)`, row.email, lr.owner.UserPostgresInfo.Id, row.token, row.age); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := userBusiness.AdmitOAuthUser(lr.ctx, "old@example.test", "Old", "google", ""); !errors.Is(err, userBusiness.ErrInvitationExpired) {
		t.Errorf("eight days old, through Google: %v, want ErrInvitationExpired", err)
	}
	rec := httptest.NewRecorder()
	authController.ValidateInvitationToken(rec, httptest.NewRequest(http.MethodGet, "/auth/validate-token?token=tok-old-legacy", nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "has expired") {
		t.Errorf("eight days old, its link: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := userBusiness.AdmitOAuthUser(lr.ctx, "recent@example.test", "Recent", "google", ""); err != nil {
		t.Errorf("two days old, through Google: %v", err)
	}
}

// Sending again renews the newest invitation for an address, by id. An
// address can have more than one row (from before inviting twice was
// refused), and renewing every row gave them all the same token, which its
// unique index refused, so such an invitation could not be sent again.
func TestRenewingTouchesOnlyTheNewestInvitation(t *testing.T) {
	lr := setupLandReady(t)
	asOwner := context.WithValue(lr.ctx, helpers.UserInfoContextKey, lr.owner)
	for _, row := range []struct{ token, age string }{{"tok-dup-old", "10 days"}, {"tok-dup-new", "9 days"}} {
		if _, err := lr.env.PG.Exec(`INSERT INTO invitations (email, invited_by, status, token, token_expires_at, created_at)
			VALUES ('dup@example.test', $1, 'sent', $2, NOW() - interval '2 days', NOW() - $3::interval)`,
			lr.owner.UserPostgresInfo.Id, row.token, row.age); err != nil {
			t.Fatal(err)
		}
	}
	tokenOf := func(age string) string {
		t.Helper()
		var token string
		if err := lr.env.PG.QueryRow(`SELECT token FROM invitations WHERE email = 'dup@example.test' AND created_at < NOW() - $1::interval
			ORDER BY created_at DESC LIMIT 1`, age).Scan(&token); err != nil {
			t.Fatal(err)
		}
		return token
	}
	for _, h := range []http.HandlerFunc{userController.AddInvitation, userController.ResendInvitation} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"email":"dup@example.test"}`)).WithContext(asOwner))
		if rec.Code != http.StatusOK {
			t.Fatalf("sending again: %d %s", rec.Code, rec.Body.String())
		}
		if old := tokenOf("9 days 12 hours"); old != "tok-dup-old" {
			t.Errorf("the older row was renewed too: token %q", old)
		}
		if newest := tokenOf("8 days"); newest == "tok-dup-new" || newest == "tok-dup-old" {
			t.Errorf("the newest row kept its token %q", newest)
		}
	}
}

// Only an admin renews an expired invitation, and the invitation is theirs
// once they do. A member who may invite learns nothing about an address:
// an account, a deactivated one, a live or an expired invitation all get the
// same refusal, so inviting can't be used to find out who is here.
func TestOnlyAdminsRenewAndMembersLearnNothing(t *testing.T) {
	lr := setupLandReady(t)
	ctx := lr.ctx
	memberID, _, err := userBusiness.CreateUserWithMethod(ctx, "member@example.test", "Member", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatal(err)
	}
	adminID, _, err := userBusiness.CreateUserWithMethod(ctx, "second.admin@example.test", "Second Admin", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := userBusiness.CreateAdminUser(ctx, "second.admin@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := userBusiness.CreateUserWithMethod(ctx, "here@example.test", "Here", nil, userModels.AuthMethodEmail, false); err != nil {
		t.Fatal(err)
	}
	goneID, _, err := userBusiness.CreateUserWithMethod(ctx, "gone@example.test", "Gone", nil, userModels.AuthMethodEmail, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := userBusiness.DeactivateUser(ctx, goneID); err != nil {
		t.Fatal(err)
	}
	lr.invite("live@example.test", "tok-live-10")
	if _, err := lr.env.PG.Exec(`INSERT INTO invitations (email, invited_by, status, token, token_expires_at)
		VALUES ('late@example.test', $1, 'sent', 'tok-late-10', NOW() - interval '1 day')`, lr.owner.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	invite := func(who userModels.UserInfo, h http.HandlerFunc, email string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"email":"`+email+`"}`)).
			WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, who)))
		var answer struct {
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return rec.Code, answer.Msg
	}
	lateRow := func() (token string, by uuid.UUID) {
		t.Helper()
		if err := lr.env.PG.QueryRow(`SELECT token, invited_by FROM invitations WHERE email = 'late@example.test'`).Scan(&token, &by); err != nil {
			t.Fatal(err)
		}
		return
	}

	member := lr.info(memberID)
	words := map[string]bool{}
	for _, email := range []string{"here@example.test", "gone@example.test", "live@example.test", "late@example.test"} {
		code, msg := invite(member, userController.AddInvitation, email)
		if code != http.StatusConflict || strings.Contains(msg, email) {
			t.Errorf("a member inviting %s: %d %q", email, code, msg)
		}
		words[msg] = true
	}
	if len(words) != 1 {
		t.Errorf("a member is told different things about different addresses: %v", words)
	}
	if token, _ := lateRow(); token != "tok-late-10" {
		t.Error("a member renewed an expired invitation")
	}
	if code, msg := invite(member, userController.AddInvitation, "fresh@example.test"); code != http.StatusOK {
		t.Errorf("a member inviting a new address: %d %q", code, msg)
	}

	// An admin is told why, and renews.
	for email, want := range map[string]string{"here@example.test": "already a member", "gone@example.test": "deactivated", "live@example.test": "is already invited"} {
		if code, msg := invite(lr.owner, userController.AddInvitation, email); code != http.StatusConflict || !strings.Contains(msg, want) {
			t.Errorf("an admin inviting %s: %d %q, want it to say %q", email, code, msg, want)
		}
	}
	if code, msg := invite(lr.owner, userController.AddInvitation, "late@example.test"); code != http.StatusOK {
		t.Fatalf("an admin renewing: %d %q", code, msg)
	}
	if token, by := lateRow(); token == "tok-late-10" || by != lr.owner.UserPostgresInfo.Id {
		t.Errorf("renewed: token %q, invited by %s, want a new token from the owner", token, by)
	}
	if code, msg := invite(lr.info(adminID), userController.ResendInvitation, "late@example.test"); code != http.StatusOK {
		t.Fatalf("another admin sending it again: %d %q", code, msg)
	}
	if _, by := lateRow(); by != adminID {
		t.Errorf("sent again by the second admin, the invitation is still %s's", by)
	}
}

// GitHub's addresses are ranked by whether a LIVE member has one. A
// deactivated account's address ranked as a member's, so someone whose old
// account was deactivated was signed in as it, and refused, though GitHub had
// verified another of their addresses that the workspace had invited.
func TestGitHubPassesOverADeactivatedAccountsAddress(t *testing.T) {
	lr := setupLandReady(t)
	goneID, _, err := userBusiness.CreateUserWithMethod(lr.ctx, "old.job@example.test", "Old Job", nil, userModels.AuthMethodGitHub, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := userBusiness.DeactivateUser(lr.ctx, goneID); err != nil {
		t.Fatal(err)
	}
	lr.invite("new.job@example.test", "tok-new-job")
	got, ok := userBusiness.GitHubSignInAddress(lr.ctx, []userBusiness.GitHubEmail{
		{Email: "old.job@example.test", Primary: true, Verified: true},
		{Email: "new.job@example.test", Verified: true},
	})
	if !ok || got != "new.job@example.test" {
		t.Errorf("chose %q (%v), want the invited address over the deactivated account's", got, ok)
	}
	// A live member's address still comes first.
	if _, _, err := userBusiness.CreateUserWithMethod(lr.ctx, "here.now@example.test", "Here Now", nil, userModels.AuthMethodGitHub, false); err != nil {
		t.Fatal(err)
	}
	got, _ = userBusiness.GitHubSignInAddress(lr.ctx, []userBusiness.GitHubEmail{
		{Email: "new.job@example.test", Primary: true, Verified: true},
		{Email: "here.now@example.test", Verified: true},
	})
	if got != "here.now@example.test" {
		t.Errorf("chose %q, want the live member's address", got)
	}
}

// A directory can stage an account before the person starts (SCIM
// active:false). It isn't welcomed then: putting it in the default channels
// told their members (user.joined) about someone who wasn't there yet. The
// welcome comes when the directory activates it, or, if it is activated some
// other way, at the person's first sign-in.
func TestAStagedAccountIsWelcomedWhenItStarts(t *testing.T) {
	lr := setupLandReady(t)
	ctx := lr.ctx
	general := lr.channel("general", false)
	staged := false
	stage := func(email string) uuid.UUID {
		t.Helper()
		created, err := scimBusiness.CreateUser(ctx, scimBusiness.ScimUserResource{UserName: email, Active: &staged}, "https://team.example.test/scim/v2")
		if err != nil {
			t.Fatalf("staging %s: %v", email, err)
		}
		id := uuid.MustParse(created.Id)
		if lr.memberOf(id, general) {
			t.Fatalf("%s was put in #general before starting", email)
		}
		return id
	}

	// Activated by the directory: welcomed then.
	sam := stage("sam.starter@example.test")
	if _, err := scimBusiness.SetActive(ctx, sam.String(), true, "https://team.example.test/scim/v2"); err != nil {
		t.Fatal(err)
	}
	if !lr.memberOf(sam, general) {
		t.Error("activated by the directory, and not in #general")
	}

	// Activated by an admin instead: welcomed at the first sign-in.
	kim := stage("kim.starter@example.test")
	if err := userBusiness.ActivateUser(ctx, kim); err != nil {
		t.Fatal(err)
	}
	if lr.memberOf(kim, general) {
		t.Fatal("an admin's activation welcomed the account; the first sign-in is meant to")
	}
	if landing := channelBusiness.FirstSignInLanding(ctx, kim); landing != general || !lr.memberOf(kim, general) {
		t.Errorf("first sign-in: lands on %s, in #general: %v", landing, lr.memberOf(kim, general))
	}
}

// The #general a workspace is seeded with is pinned by id, so new members go
// there however it is renamed. It was found by name, and renaming it sent new
// members to no channel at all.
func TestTheSeededGeneralIsFollowedThroughARename(t *testing.T) {
	lr := setupLandReady(t)
	ctx := lr.ctx
	onboardingBusiness.SeedWorkspace(ctx, lr.owner)
	var generalID uuid.UUID
	if err := lr.env.PG.QueryRow(`SELECT id FROM channels WHERE ch_name = 'general'`).Scan(&generalID); err != nil {
		t.Fatalf("the seed made no #general: %v", err)
	}
	if err := channelBusiness.UpdateChannelInfo(ctx, &channelAdapter.UpdateChannelInfo{ChannelUuid: generalID.String(), ChannelName: "everyone"}, generalID); err != nil {
		t.Fatal(err)
	}
	joined, err := userBusiness.JoinAsMember(ctx, "newcomer@example.test", "Newcomer", nil, userModels.AuthMethodGoogle, false)
	if err != nil {
		t.Fatal(err)
	}
	if !lr.memberOf(joined.UserID, generalID) || joined.Landing != generalID {
		t.Errorf("after #general became #everyone: in it %v, landing %v, want it", lr.memberOf(joined.UserID, generalID), joined.Landing)
	}
}
