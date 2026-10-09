package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	authBusiness "github.com/akashc777/OneCamp/business/Auth"
	onboardingBusiness "github.com/akashc777/OneCamp/business/Onboarding"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	business "github.com/akashc777/OneCamp/business/User"
	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/authcookie"
	"github.com/akashc777/OneCamp/initializers/oauth"
	passwordResetModels "github.com/akashc777/OneCamp/models/postgres/PasswordReset"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	authService "github.com/akashc777/OneCamp/services/Auth"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const maxPasswordLength = 72 // bcrypt truncates at 72 bytes — enforce to prevent DoS

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

func validatePassword(password string) (string, bool) {
	if len(password) < 8 {
		return "password must be at least 8 characters", false
	}
	if len(password) > maxPasswordLength {
		return fmt.Sprintf("password must not exceed %d characters", maxPasswordLength), false
	}
	return "", true
}

// EmailLogin handles POST /auth/login
func EmailLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var requestBody struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid request body",
			"status": "failed",
		})
		return
	}

	requestBody.Email = helpers.NormalizeEmail(requestBody.Email)

	if requestBody.Email == "" || requestBody.Password == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "email and password are required",
			"status": "failed",
		})
		return
	}

	// An address outside ASCII is matched to no account (see
	// helpers.NormalizeEmail): the same answer as a wrong password.
	if !helpers.AddressIsASCII(requestBody.Email) {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg":    "invalid email or password",
			"status": "failed",
		})
		return
	}

	// Note: we intentionally do NOT call validatePassword here.
	// Password complexity rules (min length, max length) are for
	// creation / change only. A user with a legacy short password
	// must still be able to log in; once logged in they can use
	// forgot-password or change-password to upgrade.

	// Get user with password hash
	user, err := domain.GetUserByEmailIdWithPassword(ctx, requestBody.Email)
	if err != nil || user == nil || user.Id == (uuid.UUID{}) {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg":    "invalid email or password",
			"status": "failed",
		})
		return
	}

	if user.PasswordHash == nil || *user.PasswordHash == "" {
		// SSO-managed users intentionally have no local password. Tell the
		// FE which auth method to surface so it can deep-link into SSO.
		method := ""
		if user.LastLoginMethod != nil {
			method = *user.LastLoginMethod
		} else if user.SignupMethod != nil {
			method = *user.SignupMethod
		}
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg":         "this account uses single sign-on. Please sign in via your provider.",
			"status":      "failed",
			"auth_method": method,
		})
		return
	}

	// Even if we somehow have a password hash on an SSO-managed user (bad
	// migration state), refuse local auth — the IdP is the source of truth.
	if user.IsSSOManaged {
		method := ""
		if user.LastLoginMethod != nil {
			method = *user.LastLoginMethod
		} else if user.SignupMethod != nil {
			method = *user.SignupMethod
		}
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg":         "this account uses single sign-on. Please sign in via your provider.",
			"status":      "failed",
			"auth_method": method,
		})
		return
	}

	// Verify password
	if err := bcrypt.CompareHashAndPassword([]byte(*user.PasswordHash), []byte(requestBody.Password)); err != nil {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg":    "invalid email or password",
			"status": "failed",
		})
		return
	}

	// A workspace that turned passwords off (AUTH_EMAIL_DISABLED=true) takes
	// them from admins only. BREAK-GLASS: when Google, GitHub or single
	// sign-on breaks (an expired client secret, an identity provider down),
	// an admin's password is the way back in to fix it, so it keeps working;
	// everyone else signs in through the provider. Checked after the password,
	// as the second step is, so an unauthenticated caller learns nothing about
	// who is an admin.
	if helpers.PasswordSignInOff() {
		isAdmin, _, err := domain.GetUserSignInFlags(ctx, user.Id)
		if err != nil {
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
				"msg":    "could not complete sign-in, please try again",
				"status": "failed",
			})
			return
		}
		if !isAdmin {
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
				"msg":    passwordsOffMessage(signInMethods(ctx), false),
				"status": "failed",
				"reason": "passwords_off",
			})
			return
		}
	}

	// A PASSWORD ALONE MUST NOT MINT A SESSION FOR AN ENROLLED USER.
	//
	// This is the entire load-bearing line of two-factor authentication, and the way to get it wrong is
	// to issue the cookies here and ask for the code on the next screen. That version looks identical to
	// a user and is worth nothing: the response already contained the session, so anyone with the
	// password is in, and the code prompt is a formality they can ignore.
	//
	// So: stop, and hand back a short-lived challenge that proves only that this step succeeded. The
	// challenge is not a session — it carries a purpose claim no middleware accepts. See
	// business/Auth.IssueTOTPChallenge, and challengeIfTwoStep for the sign-ins that ask.
	//
	// The check is ordered AFTER password verification on purpose. Asking first would tell an
	// unauthenticated caller which accounts have 2FA enabled, which is a map of who is worth attacking.
	if challengeIfTwoStep(w, ctx, user.Id, models.AuthMethodEmail) {
		return
	}

	// Audit: record this login as email-method.
	_ = domain.RecordLoginMethod(ctx, user.Id, models.AuthMethodEmail)

	// Issue JWT tokens
	issueAuthCookies(w, r, ctx, user.Id.String())
}

// challengeIfTwoStep answers a sign-in whose password just checked out, when the account has two-step
// on: with a challenge instead of a session, reporting true so the caller stops. POST /auth/login/totp
// takes the code and records method as how they signed in.
//
// Every sign-in that checks a password here asks it: the email password (EmailLogin) and the
// directory's (LDAPLogin), which skipped the second step until it did. SAML, OIDC and Google/GitHub
// don't: the identity provider authenticates the person, with whatever second factor it enforces, and
// no password reaches this server. Nor do passkeys, which are two factors already (the device, and
// what unlocks it).
//
// A FAILURE HERE REFUSES THE SIGN-IN (and still reports true, having answered) rather than falling
// through to cookies. If the database cannot say whether this user is enrolled, the safe answer is not
// "probably not": treating an unknown as unenrolled would turn a transient error into a bypass of the
// second factor.
func challengeIfTwoStep(w http.ResponseWriter, ctx context.Context, userID uuid.UUID, method string) bool {
	required, err := authBusiness.TOTPRequired(ctx, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/challengeIfTwoStep: cannot determine 2FA state: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "could not complete sign-in, please try again",
			"status": "failed",
		})
		return true
	}
	if !required {
		return false
	}
	challenge, err := authBusiness.IssueTOTPChallenge(userID, method)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/challengeIfTwoStep: cannot issue 2FA challenge: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "could not complete sign-in, please try again",
			"status": "failed",
		})
		return true
	}
	// 200, not 401: the password was correct and the client's next step is defined. The login is
	// not finished, which is what status says, and no auth cookie has been written.
	//
	// The login method is deliberately NOT recorded yet — nobody has signed in. It is recorded by
	// the challenge endpoint once the second factor is satisfied.
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status":    "totp_required",
		"msg":       "Enter the code from your authenticator app.",
		"challenge": challenge,
	})
	return true
}

// passwordsOffMessage is what someone is told when they send a password to a
// workspace that has turned passwords off: the ways in it does have, and
// only those (signInMethods). joining is for someone signing up through an
// invitation. Pure.
func passwordsOffMessage(providers map[string]bool, joining bool) string {
	var ways []string
	if providers["google"] {
		ways = append(ways, "Google")
	}
	if providers["github"] {
		ways = append(ways, "GitHub")
	}
	if providers["oidc"] || providers["saml"] {
		ways = append(ways, "single sign-on")
	}
	if providers["ldap"] {
		ways = append(ways, "your directory account")
	}
	if len(ways) == 0 {
		return "This workspace doesn't use passwords, and no other way to sign in is set up yet. Ask your administrator."
	}
	list := ways[0]
	if n := len(ways); n > 1 {
		list = strings.Join(ways[:n-1], ", ") + " or " + ways[n-1]
	}
	if joining {
		return "This workspace doesn't use passwords. Join with " + list + ", using the address you were invited at."
	}
	return "This workspace doesn't use passwords. Sign in with " + list + " instead."
}

// NoWayInProblem says why members can't sign in at all, or "" when they
// can: passwords are off (AUTH_EMAIL_DISABLED=true) and no other way in is
// set up. Admins still can, with their passwords (EmailLogin). Logged at boot
// (cmd/server) and shown by the admin's system check ("sign-in").
func NoWayInProblem(ctx context.Context) string {
	return noWayIn(signInMethods(ctx))
}

// noWayIn is NoWayInProblem for given sign-in methods. Pure.
func noWayIn(methods map[string]bool) string {
	if methods["email"] {
		return ""
	}
	for _, way := range []string{"google", "github", "oidc", "saml", "ldap"} {
		if methods[way] {
			return ""
		}
	}
	return "Passwords are off (AUTH_EMAIL_DISABLED=true) and no other way to sign in is set up, so members " +
		"can't sign in at all; only admins can, with their passwords. Set up Google, GitHub or single sign-on, " +
		"or turn passwords back on."
}

func init() {
	helpers.RegisterSystemCheck(helpers.SystemCheck{
		Name: "sign-in",
		Describe: "Members have a way to sign in: passwords, or Google, GitHub or single sign-on when passwords are off. " +
			"It does not try signing in.",
		Probe: func(ctx context.Context) error {
			if problem := NoWayInProblem(ctx); problem != "" {
				return errors.New(problem)
			}
			return nil
		},
	})
}

// signInMethods is how this workspace lets people sign in, as the sign-in
// page is told (GET /auth/providers): each method's name, and whether it is
// on.
func signInMethods(ctx context.Context) map[string]bool {
	oauthStatus := oauth.GetOAuthConfigStatus()
	return map[string]bool{
		// Email is on by default; only off with AUTH_EMAIL_DISABLED=true. It
		// was read backwards: "false" turned it off, and "true" left it on.
		"email": !helpers.PasswordSignInOff(),
		// Google + GitHub login are gated on their credentials being present
		// (DB-first, ENV-fallback — see initializers/oauth/credentials.go).
		"google": oauthStatus.GoogleConfigured,
		"github": oauthStatus.GithubConfigured,
		// Company controls the plan leaves out are reported off, so the sign-in
		// page never offers a button that would only refuse. On or off as the
		// sign-in itself reads it (helpers.EnvFlag, as the setup at boot does).
		"oidc": authService.ResolveOIDCConfig(ctx).Enabled && helpers.PlanAllows(helpers.FeatureSSO),
		"saml": authService.ResolveSAMLConfig(ctx).Enabled && helpers.PlanAllows(helpers.FeatureSSO),
		"ldap": authService.ResolveLDAPConfig(ctx).Enabled && helpers.PlanAllows(helpers.FeatureLDAP),
		"demo": helpers.DemoMode(),
	}
}

// EmailSignup handles POST /auth/signup
func EmailSignup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// No password accounts where passwords are off: the invitation admits its
	// address through Google, GitHub or single sign-on instead, which the
	// sign-up page offers.
	if helpers.PasswordSignInOff() {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg":    passwordsOffMessage(signInMethods(ctx), true),
			"status": "failed",
			"reason": "passwords_off",
		})
		return
	}

	var requestBody struct {
		Token string `json:"token"`
		// Name is what they are called, in any language: the display name.
		// Their @handle is derived from it.
		Name string `json:"name"`
		// Username is what sign-up pages before "Your name" sent; read as the
		// name when name is missing.
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid request body",
			"status": "failed",
		})
		return
	}

	name := helpers.NormalizePersonName(requestBody.Name)
	if name == "" {
		name = helpers.NormalizePersonName(requestBody.Username)
	}

	if requestBody.Token == "" || name == "" || requestBody.Password == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "Your name and a password are needed.",
			"status": "failed",
		})
		return
	}

	if msg, ok := validatePassword(requestBody.Password); !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    msg,
			"status": "failed",
		})
		return
	}

	// One rule for a person's name, the web app's (lib/validation/names.ts),
	// which the profile editor uses too: a name accepted here can be saved
	// there.
	if !helpers.IsValidPersonName(name) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    helpers.PersonNameRuleMessage,
			"status": "failed",
		})
		return
	}

	// Validate invitation token
	invitation, problem := usableInvitation(ctx, requestBody.Token)
	if problem != "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    problem,
			"status": "failed",
		})
		return
	}

	// Check if a member already has the address. An external row for it (from
	// an import or GitHub sync) isn't one: joining below adopts it.
	err, userExists := domain.CheckIfUserExistByEmail(ctx, invitation.Email)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "internal server error",
			"status": "failed",
		})
		return
	}

	if userExists {
		// The address has an account already, and signing up never touches
		// it. Sign-up used to set a password on such an account when it had
		// none (one that joined through Google or GitHub), which let whoever
		// held a live invitation link for a member's address give the account
		// a password of their choosing and sign in as its owner. The owner
		// signs in as they always have, and sets a password from their
		// profile if they want one. (Migration 204 closed the invitations
		// this left live.)
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"msg":    "You already have an account; sign in instead.",
			"status": "failed",
		})
		return
	}

	// Hash password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(requestBody.Password), bcrypt.DefaultCost)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/EmailSignup Failed to hash password err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "internal server error",
			"status": "failed",
		})
		return
	}

	// Join with a password: a new member, or the external row an import left
	// for this address, adopted with its history. Either takes a seat, and
	// puts them in the workspace's default channels.
	hashStr := string(hashedPassword)
	joined, err := business.JoinAsMember(ctx, invitation.Email, name, &hashStr, models.AuthMethodEmail, false)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/EmailSignup Failed to create user err: %+v", err)
		if helpers.WriteSeatLimit(w, err) {
			return
		}

		// Someone accepting an invitation for an address that already has an account is the one
		// failure here they can act on, and "failed to create account" tells them nothing. The
		// invitation carries the address, so this is not a guess about who they are.
		//
		// This does not disclose anything: they are holding a valid invitation token issued to
		// this exact address, so they already know it.
		if domain.IsUniqueViolationOnEmail(err) {
			helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
				"msg":    "An account already exists for this email. Try logging in instead.",
				"status": "failed",
			})
			return
		}

		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "failed to create account",
			"status": "failed",
		})
		return
	}

	// An account adopted from an import keeps the name the import gave it,
	// unless the person says otherwise, and they just did. Best effort: the
	// account works either way, and the name can be changed in their profile.
	if joined.Adopted {
		if err := business.RenameMember(ctx, joined.UserID, name); err != nil {
			helpers.LogWarnWithContext(ctx, "controllers/EmailSignup could not give the adopted account the name typed: %+v", err)
		}
	}

	// Joining marked the invitation used (JoinAsMember). Signed in, and told
	// where a new member starts (the channel they were put in, rather than an
	// empty Home) and what they are called: their name, and the @handle made
	// from it, which the page shows them.
	answer := landingField(joined.Landing)
	if answer == nil {
		answer = helpers.Envolope{}
	}
	answer["name"] = name
	answer["handle"] = joined.Handle
	issueAuthCookiesWith(w, r, ctx, joined.UserID.String(), answer)
}

// landingField is the "landing" a sign-in answers with for someone who has
// just joined (authService.NewMemberLanding), or nothing.
func landingField(landing uuid.UUID) helpers.Envolope {
	if path := authService.NewMemberLanding(landing); path != "" {
		return helpers.Envolope{"landing": path}
	}
	return nil
}

// ForgotPassword handles POST /auth/forgot-password
func ForgotPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var requestBody struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid request body",
			"status": "failed",
		})
		return
	}

	requestBody.Email = helpers.NormalizeEmail(requestBody.Email)

	// Always return success to prevent email enumeration
	successResponse := helpers.Envolope{
		"msg":    "if an account with that email exists, a password reset link has been sent",
		"status": "success",
	}

	// An address outside ASCII is matched to no account (helpers.NormalizeEmail).
	if requestBody.Email == "" || !helpers.AddressIsASCII(requestBody.Email) {
		helpers.WriteJSON(w, http.StatusOK, successResponse)
		return
	}

	// Check if user exists
	user, err := domain.GetUserByEmailId(ctx, &requestBody.Email)
	if err != nil || user == nil || user.Id == (uuid.UUID{}) {
		helpers.WriteJSON(w, http.StatusOK, successResponse)
		return
	}

	// Check that the user exists and isn't SSO-managed. OAuth users
	// (Google/GitHub) who don't have a password yet are allowed through
	// so they can set one via the reset link. SSO-managed users are
	// owned by their IdP and must authenticate there.
	userWithPw, err := domain.GetUserByEmailIdWithPassword(ctx, requestBody.Email)
	if err != nil || userWithPw == nil || userWithPw.IsSSOManaged {
		helpers.WriteJSON(w, http.StatusOK, successResponse)
		return
	}

	// Generate secure token
	token, err := generateSecureToken(32)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ForgotPassword Failed to generate token err: %+v", err)
		helpers.WriteJSON(w, http.StatusOK, successResponse)
		return
	}

	// Store token with 1-hour expiry
	expiresAt := time.Now().Add(1 * time.Hour)
	err = passwordResetModels.CreateResetToken(user.Id, token, expiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ForgotPassword Failed to create reset token err: %+v", err)
		helpers.WriteJSON(w, http.StatusOK, successResponse)
		return
	}

	// Build reset link. Resolve via the same helper used by SSO redirects so
	// FRONTEND_DOMAIN is honoured even when FE_HOST_DOMAIN isn't set.
	resetLink := fmt.Sprintf("%s/reset-password?token=%s", authService.FrontendBaseURL(), token)

	senderEmail := emailService.SenderAddress()

	// Send reset email asynchronously. The HTTP response is committed first
	// (anti-enumeration), so we log failures for operators but never expose
	// them to the caller.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				helpers.LogErrorWithContext(ctx, "controllers/ForgotPassword panic in email goroutine: %+v", r)
			}
		}()
		bgCtx := context.Background()
		sendErr := emailService.SendPasswordResetEmail(bgCtx, requestBody.Email, senderEmail, resetLink)
		if sendErr != nil {
			helpers.LogErrorWithContext(bgCtx,
				"controllers/ForgotPassword FAILED to=%s from=%s err: %+v",
				requestBody.Email, senderEmail, sendErr)
		} else {
			helpers.LogInfoWithContext(bgCtx,
				"controllers/ForgotPassword SENT to=%s from=%s", requestBody.Email, senderEmail)
		}
	}()

	helpers.WriteJSON(w, http.StatusOK, successResponse)
}

// ResetPassword handles POST /auth/reset-password
func ResetPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var requestBody struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid request body",
			"status": "failed",
		})
		return
	}

	if requestBody.Token == "" || requestBody.Password == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "token and password are required",
			"status": "failed",
		})
		return
	}

	if msg, ok := validatePassword(requestBody.Password); !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    msg,
			"status": "failed",
		})
		return
	}

	// Validate token
	resetToken, err := passwordResetModels.GetValidResetToken(requestBody.Token)
	if err != nil || resetToken == nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid or expired reset token",
			"status": "failed",
		})
		return
	}

	// Defense-in-depth: even if a stale token survived an SSO migration,
	// an SSO-managed user can't reset to a local password.
	resetUser, err := domain.GetUserAuthFlagsByUUID(ctx, resetToken.UserID)
	if err == nil && resetUser != nil && resetUser.IsSSOManaged {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg":    "this account is managed by your single sign-on provider; local passwords are disabled",
			"status": "failed",
		})
		_ = passwordResetModels.MarkTokenUsed(resetToken.ID)
		return
	}

	// Hash new password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(requestBody.Password), bcrypt.DefaultCost)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ResetPassword Failed to hash password err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "internal server error",
			"status": "failed",
		})
		return
	}

	// Update password
	hashStr := string(hashedPassword)
	err = domain.UpdatePasswordByUserID(ctx, resetToken.UserID, &hashStr)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ResetPassword Failed to update password err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "failed to update password",
			"status": "failed",
		})
		return
	}

	// Mark token as used and invalidate all other tokens for this user
	_ = passwordResetModels.MarkTokenUsed(resetToken.ID)
	_ = passwordResetModels.InvalidateAllTokensForUser(resetToken.UserID)
	// A reset is how someone takes their account back: every device signed
	// in with the old password is signed out, so a stolen session ends here.
	if err := business.EndAllSessions(ctx, resetToken.UserID.String()); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ResetPassword Failed to end sessions err: %+v", err)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "password updated successfully. You can now login with your new password.",
		"status": "success",
	})
}

// CheckAdminSetup handles GET /auth/admin-setup-required
func CheckAdminSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	exists, err := domain.CheckIfAnyAdminExists(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/CheckAdminSetup Failed to check admin err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "internal server error",
			"status": "failed",
		})
		return
	}

	// "pinned" tells the setup page to say "use the address you gave the
	// installer" rather than leaving the operator to discover the rule from a
	// refusal. It is a boolean and not the address: this endpoint answers
	// anyone, and the admin's email is not theirs to have.
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status":   "success",
		"required": !exists,
		"pinned":   settingsBusiness.InstallAdminEmail() != "",
	})
}

// AdminSetup handles POST /auth/admin-setup
func AdminSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Check if admin already exists
	exists, err := domain.CheckIfAnyAdminExists(ctx)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "internal server error",
			"status": "failed",
		})
		return
	}

	if exists {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "admin already configured",
			"status": "failed",
		})
		return
	}

	var requestBody struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Username string `json:"username"`
		// Generated is set by OneCamp Cloud, which makes the password and
		// emails it. The setup checklist then asks the owner to replace it.
		Generated bool `json:"generated"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid request body",
			"status": "failed",
		})
		return
	}

	requestBody.Email = helpers.NormalizeEmail(requestBody.Email)
	requestBody.Username = strings.TrimSpace(requestBody.Username)

	if requestBody.Email == "" || requestBody.Password == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "email and password are required",
			"status": "failed",
		})
		return
	}

	if !emailRegex.MatchString(requestBody.Email) || !helpers.AddressIsASCII(requestBody.Email) {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid email format",
			"status": "failed",
		})
		return
	}

	// The install named its admin. Nobody else claims this workspace.
	//
	// Before the password check, so a wrong address learns nothing about the
	// password rules, and with a message that says a pin exists without saying
	// what it is. See settingsBusiness.InstallAdminEmail for why this is the
	// only setting that lives in the environment alone.
	if !settingsBusiness.SetupPermittedFor(settingsBusiness.InstallAdminEmail(), requestBody.Email) {
		helpers.LogWarnWithContext(ctx,
			"controllers/AdminSetup refused: first-admin claim for an address other than the one given at install")
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg":    "This server is reserved for the email address given when it was installed. Use that address.",
			"status": "failed",
		})
		return
	}

	if msg, ok := validatePassword(requestBody.Password); !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    msg,
			"status": "failed",
		})
		return
	}

	if requestBody.Username == "" {
		requestBody.Username = strings.Split(requestBody.Email, "@")[0]
	}

	// Hash password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(requestBody.Password), bcrypt.DefaultCost)
	if err != nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "internal server error",
			"status": "failed",
		})
		return
	}

	// Create user with password
	hashStr := string(hashedPassword)
	err = business.CreateUserWithPassword(ctx, requestBody.Email, requestBody.Username, &hashStr)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/AdminSetup Failed to create user err: %+v", err)
		if helpers.WriteSeatLimit(w, err) {
			return
		}
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "failed to create admin account",
			"status": "failed",
		})
		return
	}

	// Make them admin
	err = domain.CreateAdminUser(ctx, requestBody.Email)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/AdminSetup Failed to create admin err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "user created but failed to set as admin",
			"status": "failed",
		})
		return
	}

	// Get user and issue auth cookies
	user, err := domain.GetUserByEmailId(ctx, &requestBody.Email)
	if err != nil || user == nil {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "admin created but failed to login. Please try logging in.",
			"status": "failed",
		})
		return
	}

	if requestBody.Generated {
		// Best effort: without it the checklist simply does not ask.
		_ = domain.MarkPasswordGenerated(ctx, user.Id)
		// Only OneCamp Cloud generates the first password, so this is a Cloud
		// workspace, hours before its environment says so (settings Managed):
		// nobody here is told to set up email or a model provider.
		if err := settingsBusiness.MarkManaged(); err != nil {
			helpers.LogWarnWithContext(ctx, "controllers/AdminSetup could not mark the workspace managed: %+v", err)
		}
	}

	// Give the workspace something to be, before the admin's first screen loads.
	// Setup used to end here and drop the owner into a product with nothing in
	// any of it. Best effort by construction: the account exists and works
	// regardless, and an unseeded workspace is only a slightly emptier one.
	seedNewWorkspace(ctx, user)

	issueAuthCookies(w, r, ctx, user.Id.String())
}

// seedNewWorkspace assembles the UserInfo the business layer needs and hands off
// to the seeder.
//
// The admin has no request context of their own yet: they were created moments
// ago and the middleware that normally builds this has never run for them. So it
// is assembled the same way middleware/apiToken.go does, from the two stores.
func seedNewWorkspace(ctx context.Context, user *models.User) {
	dgraphUser, err := domain.GetActiveDgraphUserInfoByUUID(ctx, user.Id.String())
	if err != nil || dgraphUser == nil {
		helpers.LogWarnWithContext(ctx,
			"controllers/AdminSetup skipping workspace seed, no dgraph user err: %+v", err)
		return
	}
	dgraphUser.IsAdmin = user.IsAdmin

	onboardingBusiness.SeedWorkspace(ctx, models.UserInfo{
		UserPostgresInfo: *user,
		UserDgraphInfo:   *dgraphUser,
	})
}

// ValidateInvitationToken handles GET /auth/validate-token?token=...
func ValidateInvitationToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	token := r.URL.Query().Get("token")
	if token == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "token is required",
			"status": "failed",
		})
		return
	}

	invitation, problem := usableInvitation(ctx, token)
	if problem != "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    problem,
			"status": "failed",
			"valid":  false,
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"valid":  true,
		"email":  invitation.Email,
		// The name to suggest: the one an import already knows them by, or ""
		// to leave the field for them.
		"name": business.NameForInvitation(ctx, invitation.Email),
		// Who invited them, and to where: the page says so, as the email did.
		"inviter_name": business.InviterName(ctx, invitation.InvitedBy),
		"workspace":    business.WorkspaceAddress(),
	})
}

// usableInvitation is the invitation a sign-up link carries, or why it
// cannot be used, in words for the person holding it (naming who to ask for
// another). One opened past its expiry is marked expired.
func usableInvitation(ctx context.Context, token string) (*models.Invitation, string) {
	invitation, err := domain.GetInvitationByToken(ctx, token)
	if err != nil || invitation == nil {
		return nil, business.InvitationLinkProblem(nil, time.Now(), "")
	}
	now := time.Now()
	problem := business.InvitationLinkProblem(invitation, now, "")
	if problem == "" || invitation.Status == business.InvitationJoined {
		return invitation, problem
	}
	if invitation.Status != business.InvitationExpired {
		_ = domain.UpdateInvitationStatus(ctx, invitation.Id, business.InvitationExpired)
	}
	return invitation, business.InvitationLinkProblem(invitation, now, business.InviterName(ctx, invitation.InvitedBy))
}

// issueAuthCookies generates JWT tokens, stores refresh in Redis, and sets auth cookies.
// Follows the exact same pattern as OAuthCallback in userController.go.
func issueAuthCookies(w http.ResponseWriter, r *http.Request, ctx context.Context, userUUID string) {
	issueAuthCookiesWith(w, r, ctx, userUUID, nil)
}

// issueAuthCookiesWith is issueAuthCookies with more to say in the answer:
// where a new member lands, say.
func issueAuthCookiesWith(w http.ResponseWriter, r *http.Request, ctx context.Context, userUUID string, extra helpers.Envolope) {
	if err := emitAuthCookiesNoBody(w, r, ctx, userUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/issueAuthCookies %v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    err.Error(),
			"status": "failed",
		})
		return
	}
	answer := helpers.Envolope{
		"msg":    "login successful",
		"status": "success",
	}
	for k, v := range extra {
		answer[k] = v
	}
	helpers.WriteJSON(w, http.StatusOK, answer)
}

// emitAuthCookiesNoBody runs the JWT mint + Redis-store + Set-Cookie sequence
// without writing a response body. Used by SSO redirect flows that follow the
// 302 success path; issueAuthCookies wraps this and writes JSON.
//
// Cookies issued: Authorization (5min), RefreshToken (TTL from Redis config),
// DeviceId (HttpOnly, refresh TTL).
func emitAuthCookiesNoBody(w http.ResponseWriter, r *http.Request, ctx context.Context, userUUID string) error {
	refreshTokenTTLDuration := registry.UserRefreshToken.TTL

	authExpiryTime := time.Now().Add(time.Minute * 6)
	authCookieExpiryTime := time.Now().Add(time.Minute * 5)
	refreshExpiryTime := time.Now().Add(refreshTokenTTLDuration)

	deviceId, err := helpers.GenerateUniqueDeviceId()
	if err != nil {
		return fmt.Errorf("failed to generate device id: %w", err)
	}
	authTokenString, refreshTokenString, err := business.StartSession(ctx, userUUID, deviceId, authExpiryTime.Unix(), refreshExpiryTime.Unix())
	if err != nil {
		return fmt.Errorf("failed to start session: %w", err)
	}

	feDomain := getFrontendCookieDomain()
	secure, sameSite := getCookieSecureAndSameSite()

	http.SetCookie(w, &http.Cookie{
		Name:     "Authorization",
		Value:    authTokenString,
		Expires:  authCookieExpiryTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   feDomain,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "RefreshToken",
		Value:    refreshTokenString,
		Expires:  refreshExpiryTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   feDomain,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "DeviceId",
		Value:    deviceId,
		Expires:  refreshExpiryTime,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
		Domain:   feDomain,
	})
	return nil
}

// getFrontendCookieDomain delegates to the canonical authcookie helper so
// all cookie-construction code uses the same domain-resolution logic.
func getFrontendCookieDomain() string {
	return authcookie.FrontendDomain()
}

// getCookieSecureAndSameSite delegates to the canonical authcookie helper.
func getCookieSecureAndSameSite() (secure bool, sameSite http.SameSite) {
	return authcookie.SecureAndSameSite()
}

func generateSecureToken(length int) (string, error) {
	b := make([]byte, length)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SetPassword handles POST /auth/set-password (authenticated)
// Allows OAuth users (who have no password) to add a password for email login.
func SetPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
	userID := userInfo.UserPostgresInfo.Id

	var requestBody struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid request body",
			"status": "failed",
		})
		return
	}

	if requestBody.Password == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "password is required",
			"status": "failed",
		})
		return
	}

	if msg, ok := validatePassword(requestBody.Password); !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    msg,
			"status": "failed",
		})
		return
	}

	// Check that user doesn't already have a password
	userEmail := userInfo.UserPostgresInfo.EmailID
	existingUser, err := domain.GetUserByEmailIdWithPassword(ctx, userEmail)
	if err != nil || existingUser == nil {
		helpers.LogErrorWithContext(ctx, "controllers/SetPassword Failed to get user err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "internal server error",
			"status": "failed",
		})
		return
	}

	if existingUser.PasswordHash != nil && *existingUser.PasswordHash != "" {
		helpers.WriteJSON(w, http.StatusConflict, helpers.Envolope{
			"msg":    "password already set. Use forgot-password to reset it.",
			"status": "failed",
		})
		return
	}

	// SSO-managed users (LDAP/SAML/OIDC) cannot add a local password — that
	// would be a backdoor around the IdP that owns the account.
	if existingUser.IsSSOManaged {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg":    "this account is managed by your single sign-on provider; local passwords are disabled",
			"status": "failed",
		})
		return
	}

	// Hash and set password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(requestBody.Password), bcrypt.DefaultCost)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SetPassword Failed to hash password err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "internal server error",
			"status": "failed",
		})
		return
	}

	hashStr := string(hashedPassword)
	err = domain.UpdatePasswordByUserID(ctx, userID, &hashStr)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/SetPassword Failed to set password err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "failed to set password",
			"status": "failed",
		})
		return
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "password set successfully. You can now login with your email and password.",
		"status": "success",
	})
}

// ChangePassword handles POST /auth/change-password (authenticated)
// For email users: requires current_password + new_password.
// For OAuth-only users (no password set): only requires new_password (acts like SetPassword).
func ChangePassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
	userID := userInfo.UserPostgresInfo.Id
	userEmail := userInfo.UserPostgresInfo.EmailID

	var requestBody struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "invalid request body",
			"status": "failed",
		})
		return
	}

	if requestBody.NewPassword == "" {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    "new password is required",
			"status": "failed",
		})
		return
	}

	if msg, ok := validatePassword(requestBody.NewPassword); !ok {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
			"msg":    msg,
			"status": "failed",
		})
		return
	}

	// Look up existing user to determine if they have a password
	existingUser, err := domain.GetUserByEmailIdWithPassword(ctx, userEmail)
	if err != nil || existingUser == nil {
		helpers.LogErrorWithContext(ctx, "controllers/ChangePassword Failed to get user err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "internal server error",
			"status": "failed",
		})
		return
	}

	// SSO-managed users cannot set or change a local password — that would
	// bypass the IdP. Same gate as SetPassword.
	if existingUser.IsSSOManaged {
		helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{
			"msg":    "this account is managed by your single sign-on provider; local passwords are disabled",
			"status": "failed",
		})
		return
	}

	hasExistingPassword := existingUser.PasswordHash != nil && *existingUser.PasswordHash != ""

	if hasExistingPassword {
		// Email user or OAuth user who already set a password — require current_password
		if requestBody.CurrentPassword == "" {
			helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{
				"msg":    "current password is required",
				"status": "failed",
			})
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(*existingUser.PasswordHash), []byte(requestBody.CurrentPassword)); err != nil {
			helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
				"msg":    "current password is incorrect",
				"status": "failed",
			})
			return
		}
	}
	// If no existing password (OAuth-only), skip current password check

	// Hash new password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(requestBody.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ChangePassword Failed to hash password err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "internal server error",
			"status": "failed",
		})
		return
	}

	hashStr := string(hashedPassword)
	err = domain.UpdatePasswordByUserID(ctx, userID, &hashStr)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ChangePassword Failed to update password err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "failed to update password",
			"status": "failed",
		})
		return
	}
	// Every other device is signed out; this one, where the password was
	// changed, stays signed in.
	keep := ""
	if dc, err := r.Cookie("DeviceId"); err == nil {
		keep = dc.Value
	}
	if err := business.EndOtherSessions(ctx, userID.String(), keep); err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/ChangePassword Failed to end other sessions err: %+v", err)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg":    "password updated successfully",
		"status": "success",
	})
}

// HasPassword handles GET /auth/has-password (authenticated)
// Returns whether the current user has a password set (used by FE to show correct UI).
func HasPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(models.UserInfo)
	userEmail := userInfo.UserPostgresInfo.EmailID

	existingUser, err := domain.GetUserByEmailIdWithPassword(ctx, userEmail)
	if err != nil || existingUser == nil {
		helpers.LogErrorWithContext(ctx, "controllers/HasPassword Failed to get user err: %+v", err)
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{
			"msg":    "internal server error",
			"status": "failed",
		})
		return
	}

	hasPassword := existingUser.PasswordHash != nil && *existingUser.PasswordHash != ""

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"has_password": hasPassword,
		"status":       "success",
	})
}

// GetEnabledProviders handles GET /auth/providers (public).
//
// Returns the set of enabled authentication methods so the FE can render the
// login page from runtime state instead of build-time NEXT_PUBLIC_* flags.
// This decouples enabling/disabling a provider from a FE redeploy.
//
// Response shape:
//
//	{
//	  "email":  true,
//	  "google": true,
//	  "github": false,
//	  "oidc":   false,
//	  "saml":   false,
//	  "ldap":   false,
//	  "demo":   false
//	}
//
// We expose only enable/disable, not credentials. Issuer URLs, tenant IDs,
// etc. are server-side only.
func GetEnabledProviders(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status":           "success",
		"providers":        signInMethods(r.Context()),
		"email_configured": emailService.IsEmailEnabled(),
		"email_sender":     emailService.SenderAddress(),
	})
}

// GetOAuthConfig returns the redacted login-OAuth credential config for the
// admin UI (GET /admin/auth/oauth-config). Secrets are never returned.
func GetOAuthConfig(w http.ResponseWriter, r *http.Request) {
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": oauth.GetOAuthConfigStatus()})
}

// UpdateOAuthConfig persists admin-entered login-OAuth credentials
// (POST /admin/auth/oauth-config). Secret fields follow omit=keep / ""=clear /
// value=set semantics; secrets are encrypted at rest. Reloads providers so the
// change takes effect without a restart.
func UpdateOAuthConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		GoogleClientID     *string `json:"google_client_id,omitempty"`
		GoogleClientSecret *string `json:"google_client_secret,omitempty"`
		GithubClientID     *string `json:"github_client_id,omitempty"`
		GithubClientSecret *string `json:"github_client_secret,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Failed to parse body"})
		return
	}

	if body.GoogleClientID != nil || body.GoogleClientSecret != nil {
		if err := oauth.SaveGoogleOAuthConfig(body.GoogleClientID, body.GoogleClientSecret); err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/UpdateOAuthConfig google err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to save Google credentials"})
			return
		}
		auditBusiness.Record(r, "auth.google_oauth", auditBusiness.CategoryAuth,
			auditBusiness.SecretChangeSummary("Google OAuth credentials", body.GoogleClientSecret), nil)
	}
	if body.GithubClientID != nil || body.GithubClientSecret != nil {
		if err := oauth.SaveGithubLoginConfig(body.GithubClientID, body.GithubClientSecret); err != nil {
			helpers.LogErrorWithContext(ctx, "controllers/UpdateOAuthConfig github err: %+v", err)
			helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Failed to save GitHub credentials"})
			return
		}
		auditBusiness.Record(r, "auth.github_oauth", auditBusiness.CategoryAuth,
			auditBusiness.SecretChangeSummary("GitHub sign-in credentials", body.GithubClientSecret), nil)
	}

	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": oauth.GetOAuthConfigStatus()})
}

// GetCollabToken returns the Authorization JWT as JSON so the FE can
// pass it to the WebSocket collaboration service. The cookie is HttpOnly
// and invisible to document.cookie, so the FE must fetch it via API.
//
// Rate-limited to 15 req/min per IP to prevent brute-force token extraction.
// The token has a 5-min TTL; the FE's onAuthenticationFailed handler
// re-fetches a fresh token on expiry.
func GetCollabToken(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("Authorization")
	if err != nil || cookie == nil || cookie.Value == "" {
		helpers.WriteJSON(w, http.StatusUnauthorized, helpers.Envolope{
			"msg":    "no authorization cookie",
			"status": "error",
		})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"status": "success",
		"token":  cookie.Value,
	})
}
