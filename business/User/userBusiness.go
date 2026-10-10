package business

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	adapterUser "github.com/akashc777/OneCamp/adapter/User"
	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	lastSeenActivityBusiness "github.com/akashc777/OneCamp/business/LastSeenActivity"
	LiveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	taskrank "github.com/akashc777/OneCamp/business/TaskRank"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	domain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	"github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/akashc777/OneCamp/initializers/oauth"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	authModels "github.com/akashc777/OneCamp/models/postgres/OAuth"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"golang.org/x/oauth2"
)

const AuthRecipeMethodGoogle = "google"
const AuthRecipeMethodGithub = "github"
const GithubUserInfoURL = "https://api.github.com/user"
const GithubUserEmailsURL = "https://api.github.com/user/emails"

const FwdMessageTypeChannel = "channel"
const FwdMessageTypeUser = "user"

const UserEmojiStatusLimit = 5

type RawFileInfo struct {
	FileName string `json:"file_name"`
}

type PostPagination struct {
	Posts   []*dgraphStruct.DgraphPost `json:"posts,omitempty"`
	HasMore bool                       `json:"has_more"`
}

type RecordingPagination struct {
	Recordings []*dgraphStruct.DgraphRecording `json:"recordings,omitempty"`
	HasMore    bool                            `json:"has_more"`
}

func CreateUser(ctx context.Context, emailID string, userName string) (err error) {

	emailID = helpers.NormalizeEmail(emailID)
	userUUID := uuid.New()
	err = domain.CreateUser(ctx, emailID, userUUID)
	zeroUnixTime := time.Time{}

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateUser Failed to create new user err: %+v",
			err)
		return
	}

	dgraphUser := &dgraphStruct.DgraphUser{
		Uuid:         userUUID.String(),
		EmailID:      emailID,
		UserName:     userName,
		UserFullName: userName,
		Status:       dgraphStruct.USER_OPT_STATUS_ONLINE,
		DeletedAt:    &zeroUnixTime,
	}

	dgraphUID, err := CreateOrUpdateDgraphUser(ctx, dgraphUser)

	if helpers.DgraphWriteFailed(dgraphUID, err) {
		helpers.LogErrorWithContext(ctx,
			"business/CreateUser Failed to create new user in dgraph err: %+v",
			err)

		// The uid was previously discarded, so a mutation that returned no uid and no error was
		// treated as success — creating a Postgres user with no Dgraph node and reporting it fine.
		if err == nil {
			err = errors.New("dgraph returned no uid for the new user")
		}

		// Reverse the Postgres insert, or this locks the person out of their own email address
		// FOREVER. users.email_id is NOT NULL UNIQUE and every user lookup reads Dgraph, so the
		// row is an account nobody can see that still owns the address — and each retry fails on
		// the constraint. Unlike a channel or project name, an email is not something the person
		// can simply choose differently.
		//
		// Safe only in this window, which is why it is here and not anywhere else: users(id) is
		// referenced by 55 tables with a mix of CASCADE, SET NULL and RESTRICT, and none of them
		// has a row for a user created moments ago.
		_ = helpers.CompensateOnFailure(ctx, "user row for "+emailID,
			func(undoCtx context.Context) error {
				return domain.HardDeleteUser(undoCtx, userUUID)
			})

		return
	}

	openSearchUser := &openSearchStruct.OpenSearchUser{
		Uuid:          userUUID.String(),
		UserName:      userName,
		UserFullName:  userName,
		UserEmail:     emailID,
		UserCreatedAt: time.Now().Unix(),
	}

	go domain.UpdateUserInOpenSearch(openSearchUser)

	return
}

// CreateExternalGitHubUser creates a stub user for an unmapped GitHub user.
func CreateExternalGitHubUser(ctx context.Context, login string, displayName string, avatarURL string, htmlURL string, email string) (*userModels.User, error) {
	userUUID := uuid.New()
	if email == "" {
		email = fmt.Sprintf("github+%s@external.onecamp.local", login)
	}

	err := domain.CreateExternalGitHubUser(ctx, userUUID, email, login, &displayName, &avatarURL, &htmlURL)
	if err != nil {
		return nil, err
	}

	zeroUnixTime := time.Time{}
	dgraphUser := &dgraphStruct.DgraphUser{
		Uuid:         userUUID.String(),
		EmailID:      email,
		UserName:     login,
		UserFullName: displayName,
		Status:       dgraphStruct.USER_OPT_STATUS_OFFLINE,
		DeletedAt:    &zeroUnixTime,
		IsExternal:   true,
	}
	if avatarURL != "" {
		dgraphUser.ProfileKey = &avatarURL
	}

	_, err = CreateOrUpdateDgraphUser(ctx, dgraphUser)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateExternalGitHubUser Failed to create dgraph user err: %+v", err)
		// Non-fatal: we still have the postgres user
	}

	// Index in OpenSearch so external users appear in global search.
	// Done after Dgraph so the search hit always corresponds to a real
	// stored user. Failure is logged + ignored — the user still exists
	// in PG/Dgraph; the next reconciliation pass can fill OS in.
	openSearchUser := &openSearchStruct.OpenSearchUser{
		Uuid:          userUUID.String(),
		UserName:      login,
		UserFullName:  displayName,
		UserEmail:     email,
		UserCreatedAt: time.Now().Unix(),
	}
	go domain.UpdateUserInOpenSearch(openSearchUser)

	return &userModels.User{
		Id:              userUUID,
		EmailID:         email,
		IsExternal:      true,
		GitHubLogin:     &login,
		GitHubAvatarURL: &avatarURL,
		GitHubHTMLURL:   &htmlURL,
		DisplayName:     &displayName,
	}, nil
}

func UploadUserFile(ctx context.Context, reader io.Reader, objectSize int64, fileName string, srcKey string, srcValue string) (u uuid.UUID, err error) {
	// Upload the test file with PutObject
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	// Magic-byte / safe-content-type validation. PrepareUpload peeks
	// the leading bytes to derive the real Content-Type, coerces
	// dangerous extensions (.html, .svg, .js, etc.) to
	// application/octet-stream, and forces Content-Disposition:
	// attachment so a malicious upload can't be served inline by the
	// browser via the presigned MinIO GET.
	safe, err := uploadsafe.PrepareUpload(reader, fileName, uploadsafe.Options{
		AllowInlineImages: true, // images are visibly previewable in chat
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("upload validation: %w", err)
	}
	u = uuid.New()
	bucketName := helpers.UserUploadBucket()
	objName := fmt.Sprintf("%v/%v_%v", userInfo.UserPostgresInfo.Id, u.String(), safe.SafeName)
	fullObjName := fmt.Sprintf("%v/%v/%v_%v", "userFileUpload", userInfo.UserPostgresInfo.Id, u.String(), safe.SafeName)

	uploadInfo, scanResult, err := attachmentBusiness.SafeUploadToMinio(ctx, safe.Reader,
		attachmentBusiness.SafeUploadOptions{
			Bucket:             bucketName,
			ObjectName:         fullObjName,
			ContentType:        safe.ContentType,
			ContentDisposition: safe.ContentDisposition,
			Size:               objectSize,
		})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UploadFile Failed to upload the file err: %+v",
			err)
		return uuid.Nil, err
	}
	_ = uploadInfo // size from MinIO is informational here
	if scanResult.Verdict == avscan.VerdictInfected {
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucketName, fullObjName,
			minio.RemoveObjectOptions{})
		helpers.LogWarnWithContext(ctx,
			"business/UploadFile rejected by AV: %s (%s)", safe.SafeName, scanResult.Signature)
		return uuid.Nil, fmt.Errorf("upload blocked: virus signature %s", scanResult.Signature)
	}

	err = attachmentBusiness.CreateAttachment(ctx, u, objName, userInfo.UserPostgresInfo.Id, srcKey, srcValue)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UploadFile Failed to add attachment to postgres err: %+v",
			err)
		// Roll back the MinIO upload so a retry doesn't leak orphans.
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucketName, fullObjName,
			minio.RemoveObjectOptions{})
		return
	}

	return
}

func UploadUserFileToChannelsAndChats(ctx context.Context, reader io.Reader, objectSize int64, fileName string, channelUUIDs []string, chatUUIDs []string) (u uuid.UUID, err error) {
	// Upload the test file with PutObject
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	safe, err := uploadsafe.PrepareUpload(reader, fileName, uploadsafe.Options{
		AllowInlineImages: true,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("upload validation: %w", err)
	}
	u = uuid.New()
	bucketName := helpers.UserUploadBucket()
	objName := fmt.Sprintf("%v/%v_%v", userInfo.UserPostgresInfo.Id, u.String(), safe.SafeName)
	fullObjName := fmt.Sprintf("%v/%v/%v_%v", "userFileUpload", userInfo.UserPostgresInfo.Id, u.String(), safe.SafeName)

	uploadInfo, scanResult, err := attachmentBusiness.SafeUploadToMinio(ctx, safe.Reader,
		attachmentBusiness.SafeUploadOptions{
			Bucket:             bucketName,
			ObjectName:         fullObjName,
			ContentType:        safe.ContentType,
			ContentDisposition: safe.ContentDisposition,
			Size:               objectSize,
		})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UploadUserFileToChannelsAndChats Failed to upload the file err: %+v",
			err)
		return uuid.Nil, err
	}
	_ = uploadInfo
	if scanResult.Verdict == avscan.VerdictInfected {
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucketName, fullObjName,
			minio.RemoveObjectOptions{})
		helpers.LogWarnWithContext(ctx,
			"business/UploadUserFileToChannelsAndChats rejected by AV: %s (%s)",
			safe.SafeName, scanResult.Signature)
		return uuid.Nil, fmt.Errorf("upload blocked: virus signature %s", scanResult.Signature)
	}

	err = attachmentBusiness.CreateAttachmentForChannelsAndChats(ctx, u, objName, userInfo.UserPostgresInfo.Id, channelUUIDs, chatUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UploadUserFileToChannelsAndChats Failed to add attachment to postgres err: %+v",
			err)
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucketName, fullObjName,
			minio.RemoveObjectOptions{})
		return
	}

	return
}

func GetRecordingURLByObjectName(ctx context.Context, objName string) (objURL string, err error) {
	bucketName := helpers.UserUploadBucket()
	// Get a pre-signed URL for the object.
	url, err := minioInit.MinioClient.PresignedGetObject(ctx, bucketName, objName, time.Duration(5*time.Minute), nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetRecordingURLByObjectName Invalid obj key",
		)
		return
	}
	objURL = url.String()
	return
}

func GetFileURLByObjectName(ctx context.Context, objName string) (objURL string, err error) {
	bucketName := helpers.UserUploadBucket()
	completeObjectName := fmt.Sprintf("%v/%v", "userFileUpload", objName)
	// Get a pre-signed URL for the object.
	url, err := minioInit.MinioClient.PresignedGetObject(ctx, bucketName, completeObjectName, time.Duration(5*time.Minute), nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetFileURLByObjectName Invalid obj key",
		)
		return
	}
	objURL = url.String()
	return
}

func GetSignedProfileURL(ctx context.Context, profileKey *string) string {
	if profileKey == nil || *profileKey == "" {
		return ""
	}

	// If the profile key is already a URL (e.g. GitHub avatar for external users),
	// return it directly instead of looking it up as a UUID in the attachment table.
	if strings.HasPrefix(*profileKey, "http://") || strings.HasPrefix(*profileKey, "https://") {
		return *profileKey
	}

	attachmentPostgresInfo, err := attachmentBusiness.GetAttachmentByObjUUID(ctx, *profileKey, postgressStruct.ATTACHMENT_SRC_PUBLIC)
	if err != nil {
		return ""
	}

	url, err := GetFileURLByObjectName(ctx, attachmentPostgresInfo.ObjKey)
	if err != nil {
		return ""
	}

	return url
}

func CheckIfUserExistByUserName(ctx context.Context, userName *string) (exists bool, err error) {

	exists, err = domain.CheckIfUserExistByUsername(ctx, userName)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CheckIfUserExistByUserName Failed to get userInfo by uname err: %+v",
			err)
		return
	}

	return
}

func GetUserByEmailId(ctx context.Context, emailId *string) (userInfo *userModels.User, err error) {

	userInfo, err = domain.GetUserByEmailId(ctx, emailId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUserByEmailId Failed to get userInfo by email err: %+v",
			err)
		return
	}

	return
}

// StartSession starts a signed-in session on a device: a short-lived access
// token, and the refresh token that renews it (RotateRefreshToken), kept under
// the person and device so only the latest one is honoured. Every way of
// signing in ends here.
func StartSession(ctx context.Context, userID, deviceID string, authExp, refreshExp int64) (authToken, refreshToken string, err error) {
	authToken, err = GenerateAuthTokenString(ctx, userID, authExp)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/StartSession Failed to create auth token err: %+v", err)
		return "", "", errors.New("failed to create auth token")
	}
	refreshToken, err = GenerateRefreshTokenString(ctx, userID, refreshExp)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/StartSession Failed to create refresh token err: %+v", err)
		return "", "", errors.New("failed to create refresh token")
	}
	if err = redisStore.SetString(ctx, registry.UserRefreshToken, []string{userID, deviceID}, refreshToken); err != nil {
		helpers.LogErrorWithContext(ctx, "business/StartSession Failed to set refresh token in redis err: %+v", err)
		return "", "", errors.New("failed to set refreshToken in redis")
	}
	return authToken, refreshToken, nil
}

func GetUserRecordingsList(ctx context.Context, userDgraphUID string, startDate string, endDate string, pageIndex int, pageSize int) (recordingsPagination RecordingPagination, err error) {
	dgraphRecording, err := domain.GetUserRecordingsList(ctx, userDgraphUID, startDate, endDate, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUserRecordingsList Failed to get user recording from dgraph err: %+v",
			err,
		)
		err = errors.New("failed to get users resording from dgraph")
		return
	}

	if len(dgraphRecording) > pageSize {
		recordingsPagination.Recordings = dgraphRecording[:pageSize]
	} else {
		recordingsPagination.Recordings = dgraphRecording
	}

	recordingsPagination.HasMore = len(dgraphRecording) > pageSize

	return
}

func LoginUserByEmailID(ctx context.Context, emailID string, uname string, authTokenExpiryUnix int64, refreshTokenExpiryUnix int64) (userInfo *dgraphStruct.DgraphUser, AuthTokenString string, RefreshTokenString string, deviceId string, err error) {

	// get userInfo by emailID
	userInfo, err = domain.GetDgraphUserInfoByEmailId(ctx, emailID)
	if userInfo == nil || userInfo.Uuid == "" {
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/LoginUserByEmailID Failed to get userInfo by emailID err: %+v",
			err)
		return
	}

	// check email
	// if uname != userInfo.UserName {
	// 	err = UpdateUserName(ctx, userInfo.Uuid, uname)
	// 	if err != nil {
	// 		helpers.LogErrorWithContext(ctx,
	// 			"business/LoginUserByEmailID failed to update uname",
	// 		)
	// 		return

	// 	}
	// }

	deviceId, err = helpers.GenerateUniqueDeviceId()
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/LoginUserByEmailID Failed to generate deviceId err:= %+v",
			err,
		)
		err = errors.New("Failed to generate deviceId")
		return
	}
	AuthTokenString, RefreshTokenString, err = StartSession(ctx, userInfo.Uuid, deviceId, authTokenExpiryUnix, refreshTokenExpiryUnix)
	if err != nil {
		return
	}

	openSearchUser := &openSearchStruct.OpenSearchUser{
		Uuid:           userInfo.Uuid,
		UserName:       userInfo.UserName,
		UserEmail:      userInfo.EmailID,
		UserProfileKey: userInfo.ProfileKey,
	}

	go domain.UpdateUserInOpenSearch(openSearchUser)

	return

}

func GenerateRefreshTokenString(ctx context.Context, userId string, exp int64) (RefreshTokenString string, err error) {
	// Typed, so a refresh token is never taken for a session (helpers/sessionToken.go).
	RefreshTokenString, err = helpers.SignSessionToken(userId, helpers.TokenTypeRefresh, exp)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GenerateRefreshTokenString Failed to generate auth token err: %+v",
			err,
		)
		return
	}
	return
}

func GenerateAuthTokenString(ctx context.Context, userId string, exp int64) (AuthTokenString string, err error) {
	AuthTokenString, err = helpers.SignSessionToken(userId, helpers.TokenTypeAccess, exp)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GenerateAuthTokenString Failed to generate auth token err: %+v",
			err,
		)
		return
	}
	return
}

func OAuthLogin(ctx context.Context, oauthStateString string, provider string) (url string, err error) {
	switch provider {
	case AuthRecipeMethodGoogle:
		// during the init of OAuthProvider authorizer url might be empty
		cfg := oauth.GoogleConfig()
		if cfg == nil {
			return "", errors.New("google login is not configured")
		}
		url = cfg.AuthCodeURL(oauthStateString, oauth2.AccessTypeOffline)

	case AuthRecipeMethodGithub:
		cfg := oauth.GithubConfig()
		if cfg == nil {
			return "", errors.New("github login is not configured")
		}
		url = cfg.AuthCodeURL(oauthStateString, oauth2.AccessTypeOffline)

	default:
		helpers.LogErrorWithContext(ctx,
			"controllers/OAuthCallback Unknown provider : %+v",
			provider)
		err = errors.New("unknown provider")
	}

	return
}

// OAuthCallback reads who Google or GitHub signed in, and admits them. landing
// is the channel someone who has just joined starts in (Joined.Landing), and
// uuid.Nil for a member signing in again.
func OAuthCallback(ctx context.Context, oauthCode string, provider string) (emailId string, uname string, landing uuid.UUID, err error) {
	var user *authModels.User
	// The Google Workspace domain that manages a Google account (the ID
	// token's hd), or "": what a domain entry on the allow-list admits by.
	hostedDomain := ""
	switch provider {
	case AuthRecipeMethodGoogle:
		user, hostedDomain, err = processGoogleUserInfo(ctx, oauthCode)
	case AuthRecipeMethodGithub:
		user, err = processGithubUserInfo(ctx, oauthCode)
	default:
		err = errors.New("unknown provider")
	}

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/OAuthCallback Failed to process user info err: %+v",
			err)

		return
	}
	if user == nil || user.Email == nil || strings.TrimSpace(*user.Email) == "" {
		err = ErrUnverifiedEmail
		return
	}

	emailId = helpers.NormalizeEmail(*user.Email)
	if !helpers.AddressIsASCII(emailId) {
		err = ErrAddressNotASCII
		return
	}
	uname = displayName(emailId, user.GivenName)

	joined, err := AdmitOAuthUser(ctx, emailId, uname, provider, hostedDomain)
	landing = joined.Landing
	return
}

// AdmitOAuthUser decides whether the person Google or GitHub just vouched for
// may sign in with that address. A member may, and gets a zero Joined. Anyone
// else needs to be on the allow-list or invited, and then joins
// (JoinAsMember), taking a seat.
//
// "Anyone else" includes the owner of an external row: an import or GitHub
// sync makes one for an address so its history has an author. Any row with the
// address used to count as an account, so that row's owner skipped the
// invitation and signed in as an external who never took a seat.
//
// hostedDomain is the Google Workspace domain that manages a Google account
// (the ID token's hd claim), or "": a domain entry on the allow-list admits
// by it alone (admittedByDomain).
func AdmitOAuthUser(ctx context.Context, emailID string, name string, provider string, hostedDomain string) (Joined, error) {
	// Not matched at all, so a lookup that folds case the Unicode way can't
	// find someone else's account for it.
	if emailID = helpers.NormalizeEmail(emailID); !helpers.AddressIsASCII(emailID) {
		return Joined{}, ErrAddressNotASCII
	}
	err, member := domain.CheckIfUserExistByEmail(ctx, emailID)
	if err != nil {
		return Joined{}, err
	}
	if member {
		return Joined{}, nil
	}

	if !allowListed(emailID) && !allowListedDomain(emailID, provider, hostedDomain) {
		// An invitation admits only while it is live: not used, and not past
		// its expiry. Any invitation row at all used to, so an expired one, or
		// one used by somebody signing up with the link, still let its address
		// in through Google or GitHub. A failed check is not a "no": the
		// person is told they aren't invited only when that is known.
		usable, invErr := domain.HasUsableInvitation(ctx, emailID)
		if invErr != nil {
			return Joined{}, invErr
		}
		if !usable {
			if inv, _ := domain.GetInvitationByEmail(ctx, emailID); inv != nil && inv.Status != InvitationJoined {
				return Joined{}, ErrInvitationExpired
			}
			return Joined{}, ErrNotInvited
		}
	}

	// Tag the new user with their signup method ("google" / "github").
	// OAuth users are NOT marked is_sso_managed=true — they can still
	// add a local password (SetPassword) for break-glass cases. Only
	// LDAP/SAML/OIDC users are SSO-managed.
	joined, err := JoinAsMember(ctx, emailID, name, nil, provider, false)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/AdmitOAuthUser Failed to create user err: %+v",
			err)
		return Joined{}, err
	}
	return joined, nil
}

// processGoogleUserInfo reads who Google signed in, and the Google Workspace
// domain that manages the account (hd), "" for an account no organisation
// manages.
func processGoogleUserInfo(ctx context.Context, code string) (*authModels.User, string, error) {
	googleCfg := oauth.GoogleConfig()
	googleOIDC := oauth.GoogleOIDCProvider()
	if googleCfg == nil || googleOIDC == nil {
		return nil, "", fmt.Errorf("google login is not configured")
	}
	oauth2Token, err := googleCfg.Exchange(ctx, code)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/processGoogleUserInfo Failed to exchange code for token err: %+v",
			err)
		return nil, "", fmt.Errorf("invalid google exchange code: %s", err.Error())
	}
	verifier := googleOIDC.Verifier(&oidc.Config{ClientID: googleCfg.ClientID})

	// Extract the ID Token from OAuth2 token.
	rawIDToken, ok := oauth2Token.Extra("id_token").(string)
	if !ok {
		helpers.LogErrorWithContext(ctx,
			"business/processGoogleUserInfo Failed to extract ID Token from OAuth2 token err: %+v",
			err)
		return nil, "", fmt.Errorf("unable to extract id_token")
	}

	// Parse and verify ID Token payload.
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/processGoogleUserInfo Failed to verify ID Token err: %+v",
			err)
		return nil, "", fmt.Errorf("unable to verify id_token: %s", err.Error())
	}
	var claims struct {
		Email         string      `json:"email"`
		EmailVerified interface{} `json:"email_verified"`
		GivenName     string      `json:"given_name"`
		Name          string      `json:"name"`
		// HostedDomain is the Google Workspace domain that manages the
		// account; absent for an account no organisation manages.
		HostedDomain string `json:"hd"`
	}
	if err := idToken.Claims(&claims); err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/processGoogleUserInfo Failed to parse ID Token claims err: %+v",
			err)
		return nil, "", fmt.Errorf("unable to extract claims")
	}
	email := strings.TrimSpace(claims.Email)
	if email == "" || !helpers.TrueClaim(claims.EmailVerified) {
		return nil, "", ErrUnverifiedEmail
	}
	name := displayName(email, &claims.GivenName, &claims.Name)
	return &authModels.User{Email: &email, GivenName: &name}, strings.TrimSpace(claims.HostedDomain), nil
}

func processGithubUserInfo(ctx context.Context, code string) (*authModels.User, error) {
	githubCfg := oauth.GithubConfig()
	if githubCfg == nil {
		return nil, fmt.Errorf("github login is not configured")
	}
	oauth2Token, err := githubCfg.Exchange(ctx, code)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/processGithubUserInfo Failed to exchange code for token err: %+v",
			err)
		return nil, fmt.Errorf("invalid github exchange code: %s", err.Error())
	}
	client := &http.Client{Timeout: 30 * time.Second}

	var profile struct {
		Login string  `json:"login"`
		Name  *string `json:"name"`
	}
	if err := githubGet(ctx, client, oauth2Token.AccessToken, GithubUserInfoURL, &profile); err != nil {
		helpers.LogErrorWithContext(ctx, "business/processGithubUserInfo Failed to read the github user err: %+v", err)
		return nil, fmt.Errorf("failed to request github user info")
	}
	// The list, not the profile's public address: only the list says which
	// addresses GitHub has verified. Any of them may be the one this
	// workspace knows.
	var emails []GitHubEmail
	if err := githubGet(ctx, client, oauth2Token.AccessToken, GithubUserEmailsURL, &emails); err != nil {
		helpers.LogErrorWithContext(ctx, "business/processGithubUserInfo Failed to read the github user's emails err: %+v", err)
		return nil, fmt.Errorf("failed to request github user emails")
	}
	email, ok := GitHubSignInAddress(ctx, emails)
	if !ok {
		return nil, ErrUnverifiedEmail
	}
	name := displayName(email, profile.Name, &profile.Login)
	return &authModels.User{Email: &email, GivenName: &name}, nil
}

func GetAllAdminUsers(ctx context.Context, pageIndex int, pageSize int) (usersInfo []*userModels.User, hasMore bool, err error) {
	usersInfo, actualLen, err := domain.GetAllAdminUsers(ctx, pageIndex, pageSize)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetAllAdminUsers Failed to get all admin userInfo err: %+v",
			err,
		)
		err = errors.New("failed to get all admin userInfo")
		return
	}

	if actualLen > pageSize {
		hasMore = true
		usersInfo = usersInfo[:pageSize]
	}

	return
}

func HardDeleteAdminUserByEmailId(ctx context.Context, emailId string, userUUID ...string) (err error) {
	err = domain.HardDeleteAdminUserByEmailId(ctx, emailId, userUUID...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/HardDeleteAdminUserByEmailId Failed to delete admin user err: %+v",
			err,
		)
		err = errors.New("failed delete admin user")
		return
	}

	return
}

func CreateAdminUser(ctx context.Context, emailId string, userUUID ...string) (err error) {
	err = domain.CreateAdminUser(ctx, emailId, userUUID...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateAdminUser Failed to create admin user err: %+v",
			err,
		)
		err = errors.New("failed create admin user")
		return
	}

	return
}

func GetAllInvitations(ctx context.Context) (invitations []*userModels.Invitation, err error) {

	invitations, err = domain.GetAllInvitations(ctx)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetAllInvitations Failed to get invitations err: %+v", err)
		err = errors.New("failed to get invitations")
		return
	}
	return
}

func DeleteInvitationByEmail(ctx context.Context, email string) (err error) {
	err = domain.DeleteInvitationByEmail(ctx, email)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DeleteInvitationByEmail Failed to delete invitation err: %+v", err)
		err = errors.New("failed to delete invitation")
		return
	}
	return
}

func DeactivateUser(ctx context.Context, userUUID uuid.UUID) (err error) {

	currentTime := time.Now()
	err = domain.UpdateDeletedTimeByUUID(ctx, &currentTime, &currentTime, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeactivateUser Failed to delete user err: %+v",
			err,
		)
		err = errors.New("failed to delete user")
		return
	}

	dgraphInfo := &dgraphStruct.DgraphUser{
		Uuid:      userUUID.String(),
		Uid:       "uid(user)",
		DeletedAt: &currentTime,
		UpdatedAt: &currentTime,
	}

	_, err = domain.CreateOrUpdateDgraphUser(ctx, dgraphInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/DeactivateUser Failed to update delete time in user dgraph err: %+v",
			err,
		)
		err = errors.New("failed to delete user")
		return
	}

	return

}

func AddFavChannel(ctx context.Context, userUUID string, channelDgraphUid string) (err error) {
	dgraphInfo := &dgraphStruct.DgraphUser{
		Uuid: userUUID,
		Uid:  "uid(user)",
		FavChannels: []*dgraphStruct.DgraphChannel{{
			Uid: channelDgraphUid,
		}},
	}

	_, err = domain.CreateOrUpdateDgraphUser(ctx, dgraphInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/AddFavChannel Failed to add fav channel in user dgraph err: %+v",
			err,
		)
		err = errors.New("failed to add fav channel")
		return
	}

	return
}

func RemoveFavChannel(ctx context.Context, userDgraphUID string, channelDgraphUID string) (err error) {

	err = domain.DeleteFavChannelEdge(ctx, userDgraphUID, channelDgraphUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/RemoveFavChannel Failed to remove fav channel in user dgraph err: %+v",
			err,
		)
		err = errors.New("failed to remove fav channel")
		return
	}

	return
}

func ActivateUser(ctx context.Context, userUUID uuid.UUID) (err error) {

	zeroUnixTime := time.Time{}.UTC()
	currentTime := time.Now()
	err = domain.UpdateDeletedTimeToNullByUUID(ctx, &currentTime, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/ActivateUser Failed to delete user err: %+v",
			err,
		)
		err = errors.New("failed to delete user")
		return
	}

	dgraphInfo := &dgraphStruct.DgraphUser{
		Uuid:      userUUID.String(),
		Uid:       "uid(user)",
		DeletedAt: &zeroUnixTime,
		UpdatedAt: &currentTime,
	}

	_, err = domain.CreateOrUpdateDgraphUser(ctx, dgraphInfo)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/ActivateUser Failed to update delete time in user dgraph err: %+v",
			err,
		)
		err = errors.New("failed to delete user")
		return
	}

	return

}

func GetAllUsersListFromDgraph(ctx context.Context, pageIndex int, pageSize int) (dgraphUsers []*dgraphStruct.DgraphUser, hasMore bool, err error) {
	dgraphUsers, actualLen, err := domain.GetAllUsersListFromDgraph(ctx, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetAllUsersListFromDgraph Failed to get users from dgraph err: %+v",
			err,
		)
		return
	}

	if actualLen > pageSize {
		hasMore = true
		dgraphUsers = dgraphUsers[:pageSize]
	}
	// So the admin's list can show and find each member by @handle.
	domain.AttachHandles(ctx, dgraphUsers)
	return
}

func UpdateUserStatusInDgraph(ctx context.Context, status string, userUUID string) (err error) {

	dgraphUser := &dgraphStruct.DgraphUser{
		Uid:    "uid(user)",
		Status: status,
		Uuid:   userUUID,
	}

	_, err = domain.CreateOrUpdateDgraphUser(ctx, dgraphUser)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"businessUpdateUserStatusInDgraph Failed to update user's status in dgraph err: %+v",
			err,
		)
		return
	}

	mqttEmojiStatus := mqttStruct.MqttUserStatus{
		Type:     mqttStruct.TYPE_DELETE,
		UserUuid: userUUID,
		Status:   status,
	}

	go mqttBusiness.PublishUserStatus(&mqttEmojiStatus)

	return
}

func UpdateUserTheme(ctx context.Context, userUUID string, themeColor string, themeMode string) (err error) {
	dgraphUser := &dgraphStruct.DgraphUser{
		Uid:        "uid(user)",
		Uuid:       userUUID,
		ThemeColor: themeColor,
		ThemeMode:  themeMode,
	}

	_, err = domain.CreateOrUpdateDgraphUser(ctx, dgraphUser)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateUserTheme Failed to update user's theme in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func CreateOrUpdateDgraphUser(ctx context.Context, dgraphUser *dgraphStruct.DgraphUser) (newUserUid string, err error) {
	// READ THE PREVIOUS NAME/AVATAR BEFORE THE UPSERT, because the comparison below is the whole
	// point and the upsert would otherwise overwrite the values it compares against — leaving the
	// old and new always equal, the propagation never firing, and every search result stuck with
	// a name the user has already changed.
	previous, prevErr := domain.GetDgraphUserInfoByUUID(ctx, dgraphUser.Uuid)

	newUserUid, err = domain.CreateOrUpdateDgraphUser(ctx, dgraphUser)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateUserInDgraph Failed to create user in dgraph err: %+v",
			err,
		)
		err = errors.New("failed to create user in dgraph")
		return
	}

	// Sync with OpenSearch
	openSearchUser := &openSearchStruct.OpenSearchUser{
		Uuid:           dgraphUser.Uuid,
		UserName:       dgraphUser.UserName,
		UserFullName:   dgraphUser.UserFullName,
		UserProfileKey: dgraphUser.ProfileKey,
		UserUpdatedAt:  time.Now().Unix(),
	}
	go domain.UpdateUserInOpenSearch(openSearchUser)

	// Propagate the denormalised copies of the name people see (shownNameAfter) and profile pic across every index that
	// embeds them — ONLY IF ONE OF THEM ACTUALLY CHANGED.
	//
	// This used to run on every call. The only live caller is POST /updateUserProfile, which
	// submits the WHOLE profile, so saving a job title, hobbies, app language or status — or
	// pressing save with nothing edited — rewrote every post, comment and task document that
	// mentions the user with values identical to the ones already there.
	//
	// That is not merely wasted work. Each rewrite bumps the document's seqNo, so two saves in
	// quick succession produced two concurrent update_by_query passes over the same ~130
	// documents, and beta logged the collision: total 9 / updated 0 / version_conflicts 9 on one
	// pass while another reported updated 71 of the same 111 comments. The conflicts were a
	// symptom; propagating a change that had not happened was the cause.
	profilePic := ""
	if dgraphUser.ProfileKey != nil {
		profilePic = *dgraphUser.ProfileKey
	}

	// The read above is tolerated failing: it happens before a write that has now succeeded, so
	// its failure must not turn a completed profile update into an error. When the previous state
	// is unknown, propagate — a stale name in search is worse than a redundant pass.
	switch {
	case prevErr != nil:
		helpers.LogInfoWithContext(ctx,
			"business/CreateOrUpdateDgraphUser could not read the previous profile for %s (%v); "+
				"propagating anyway rather than risk a stale name in search", dgraphUser.Uuid, prevErr)
		if name := shownNameAfter(nil, dgraphUser); name != "" {
			go domain.PropagateUserInfoInOpenSearch(dgraphUser.Uuid, name, profilePic)
		}
	case previous == nil:
		// Defensive only. A missing user comes back as an ERROR from the read, not as a nil with
		// no error, so a creation lands in the branch above rather than here — and the sole live
		// caller is /updateUserProfile, where the user necessarily exists. Kept because a nil
		// with no error would otherwise dereference below, not because it is a path we expect.
	default:
		prevProfilePic := ""
		if previous.ProfileKey != nil {
			prevProfilePic = *previous.ProfileKey
		}
		name := shownNameAfter(previous, dgraphUser)
		if name != previous.DisplayName() || prevProfilePic != profilePic {
			go domain.PropagateUserInfoInOpenSearch(dgraphUser.Uuid, name, profilePic)
		}
	}

	return
}

// shownNameAfter is the name people see for someone (DgraphUser.DisplayName)
// once update is written over previous: an empty field in update leaves the
// saved one as it is (the graph write omits it). previous may be nil.
//
// The search index's copies of an author's name are that name. A rename used
// to copy the full name over them, and only when the full name changed, so
// changing the display name left every search result with the old one, and
// changing the full name replaced the display name there. Pure.
func shownNameAfter(previous, update *dgraphStruct.DgraphUser) string {
	merged := dgraphStruct.DgraphUser{}
	if previous != nil {
		merged.UserName, merged.UserFullName, merged.EmailID = previous.UserName, previous.UserFullName, previous.EmailID
	}
	if update != nil {
		if update.UserName != "" {
			merged.UserName = update.UserName
		}
		if update.UserFullName != "" {
			merged.UserFullName = update.UserFullName
		}
		if update.EmailID != "" {
			merged.EmailID = update.EmailID
		}
	}
	return merged.DisplayName()
}

func GetUsersPosts(ctx context.Context, userUUID string, userDgraphUID string, pageIndex int, pageSize int) (userPostPagination PostPagination, err error) {

	dgraphUser, err := domain.GetUserPostsByUserUUID(ctx, userUUID, userDgraphUID, pageIndex, pageSize)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUsersPosts Failed to get user post from dgraph err: %+v",
			err,
		)
		err = errors.New("failed to get users post from dgraph")
		return
	}

	if len(dgraphUser.Posts) > pageSize {
		userPostPagination.Posts = dgraphUser.Posts[:pageSize]
	} else {
		userPostPagination.Posts = dgraphUser.Posts
	}

	userPostPagination.HasMore = len(dgraphUser.Posts) > pageSize

	return
}

func UsersListNotExistInGivenChannel(ctx context.Context, channelUUID string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {

	dgraphUsers, err = domain.DgraphUsersListNotExistInGivenChannel(ctx, channelUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UsersListNotExistInGivenChannel Failed to get users list from dgraph err: %+v",
			err,
		)
		err = errors.New("failed to get users list from dgraph")
		return
	}

	return
}

func GetDgraphAllUsersList(ctx context.Context) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {

	dgraphUsers, err = domain.GetDgraphAllUsersList(ctx)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UsersListNotExistInGivenChannel Failed to get users list from dgraph err: %+v",
			err,
		)
		err = errors.New("failed to get users list from dgraph")
		return
	}
	// The @mention picker searches this list by display name, full name and
	// handle, and handles are in Postgres.
	domain.AttachHandles(ctx, dgraphUsers)

	return
}

func ClearUserEmojiStatus(ctx context.Context, statusDgraphUID string, userUUID string) (err error) {

	currentTime := time.Now()

	dgraphUserEmojiStatus := &dgraphStruct.DgraphUserStatusEmoji{
		Uid:      statusDgraphUID,
		ExpiryAt: &currentTime,
	}

	err = domain.UpdateDgraphUserStatus(ctx, dgraphUserEmojiStatus, userUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/ClearUserEmojiStatus Failed to clear user's emoji status from dgraph err: %+v",
			err,
		)
		err = errors.New("failed to clear user's emoji status from dgraph")
		return
	}

	mqttEmojiStatus := mqttStruct.MqttUserEmojiStatus{
		Type:     mqttStruct.TYPE_DELETE,
		UserUuid: userUUID,
	}

	go mqttBusiness.PublishUserEmojiStatus(&mqttEmojiStatus)

	return

}

func GetAllUserEmojiStatusList(ctx context.Context, userUUID string) (dgraphUserStatusesInfo *dgraphStruct.DgraphUser, err error) {

	dgraphUserStatusesInfo, err = domain.GetAllUserEmojiStatusList(ctx, userUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetAllUserEmojiStatusList Failed to get user's emoji status from dgraph err: %+v",
			err,
		)
		err = errors.New("failed to get user's emoji status from dgraph")
		return
	}

	return
}

func UpdateUserEmojiStatus(ctx context.Context, userDgraphInfo *dgraphStruct.DgraphUser, userEmojiStatusEmoji *adapterUser.UserEmojiStatusInput) (err error) {

	if userEmojiStatusEmoji == nil {
		return
	}

	dgraphUserStatusesInfo, err := domain.GetAllUserEmojiStatusList(ctx, userDgraphInfo.Uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateUserEmojiStatus Failed to get user emoji status from dgraph err: %+v",
			err,
		)
		err = errors.New("failed to get user emoji status from dgraph")
		return
	}

	var expiryTime time.Time

	if userEmojiStatusEmoji.ExpiryTimeAt != "" {
		var epochInt int64
		epochInt, err = strconv.ParseInt(userEmojiStatusEmoji.ExpiryTimeAt, 10, 64)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateUserEmojiStatus Error parsing expiry string to int64 err: %+v", err)
			return
		}

		expiryTime = time.Unix(epochInt, 0)
	}

	if userEmojiStatusEmoji.ExpiryTimeAt == "" {

		var loc *time.Location

		loc, err = time.LoadLocation(userEmojiStatusEmoji.TimeZone)

		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateUserEmojiStatus Error get timezone err: %+v", err)
			return
		}

		expiryTime = helpers.GetUserEmojiExipryTime(userEmojiStatusEmoji.ExpiryTimeIn, loc)
	}

	if expiryTime.IsZero() {
		err = errors.New("expiry time is not set")
		helpers.LogErrorWithContext(ctx, "business/UpdateUserEmojiStatus failed to get expiry time")
		return
	}

	dgraphUserUpdate := &dgraphStruct.DgraphUser{
		Uuid: userDgraphInfo.Uuid,
		StatusEmoji: []*dgraphStruct.DgraphUserStatusEmoji{{
			DType:     []string{"Status"},
			EmojiUuid: userEmojiStatusEmoji.EmojiUuid,
			EmojiDesc: userEmojiStatusEmoji.StatusDesc,
			ExpiryAt:  &expiryTime,
			ExpiryIn:  userEmojiStatusEmoji.ExpiryTimeIn,
		}},
	}

	if len(dgraphUserStatusesInfo.StatusEmoji) == UserEmojiStatusLimit {
		dgraphUserUpdate.StatusEmoji[0].Uid = dgraphUserStatusesInfo.StatusEmoji[UserEmojiStatusLimit-1].Uid
	}

	_, err = domain.CreateOrUpdateDgraphUser(ctx, dgraphUserUpdate)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateUserEmojiStatus Error updating emoji statuses err: %+v", err)
		return
	}

	mqttEmojiStatus := mqttStruct.MqttUserEmojiStatus{
		Type:        mqttStruct.TYPE_UPDATE,
		UserUuid:    userDgraphInfo.Uuid,
		EmojiStatus: dgraphUserUpdate.StatusEmoji[0],
	}

	go mqttBusiness.PublishUserEmojiStatus(&mqttEmojiStatus)

	return
}

func GetUserByUUID(ctx context.Context, userUUID uuid.UUID) (userInfo *userModels.User, err error) {
	userInfo, err = domain.GetUserByUUID(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUserByUUID Failed to get user by uuid err: %+v",
			err,
		)
		err = errors.New("failed to get user by uuid")
		return
	}

	return

}

func GetDgraphUserInfoByUUID(ctx context.Context, userUUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	dgraphUser, err = domain.GetDgraphUserInfoByUUID(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphUserInfoByUUID Failed to get dgraph user info by uuid err: %+v",
			err,
		)
		return
	}

	// Stamped here rather than in each controller: this is the one function
	// every profile view goes through, and a caller that has to remember to
	// classify is a caller that will eventually forget and fall back to
	// describing some other bot as the assistant.
	if dgraphUser != nil {
		dgraphUser.BotKind = string(domain.ClassifyBot(dgraphUser.EmailID))
	}

	return

}

func GetDmCallStatus(ctx context.Context, grpId string) (exists bool, err error) {
	// "Call active" means a human is in the room — not merely that the room
	// object exists. The transcription agent auto-joins every room and
	// LiveKit keeps an empty room alive for its EmptyTimeout, so a plain
	// existence check (CheckRoomExists) reports a DM call as active long
	// after both humans have hung up — and, for DMs specifically, can leave
	// the call dot stuck on indefinitely.
	exists, err = LiveKitBusiness.IsCallActive(ctx, grpId)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDmCallStatus Failed to chek my DM call exists err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphUserProjectList(ctx context.Context, userUUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	dgraphUser, err = domain.GetDgraphUserProjectList(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphUserProjectList Failed to get dgraph user info by uuid err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphUserTaskListForKanban(ctx context.Context, userUUID string, userDgraphUID string, filterQuery string, closedLimit int) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	dgraphUser, err = domain.GetDgraphUserTaskListForKanban(ctx, userUUID, userDgraphUID, filterQuery, closedLimit)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphUserTaskListForKanban Failed to get user's task list in dgraph err: %+v",
			err,
		)
		return
	}
	if dgraphUser != nil {
		for _, column := range [][]*dgraphStruct.DgraphTask{dgraphUser.TasksTodo, dgraphUser.TasksInProgress, dgraphUser.TasksBacklog, dgraphUser.TasksInReview, dgraphUser.TasksCanceled, dgraphUser.TasksDone} {
			taskrank.Sort(column)
		}
	}

	return
}

func GetDgraphUserTaskList(ctx context.Context, userUUID string, userDgraphUID string, filterQuery string, sortQuery string, pageSize int, pageIndex int, getAll bool) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	dgraphUser, err = domain.GetDgraphUserTaskList(ctx, userUUID, userDgraphUID, filterQuery, sortQuery, pageSize, pageIndex, getAll)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphUserTaskList Failed to get user's task list in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphUserInfoByUUIDForSidebarNav(ctx context.Context, userUUID uuid.UUID) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	dgraphUser, err = domain.GetDgraphUserInfoByUUIDForSidebarNav(ctx, userUUID.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphUserInfoByUUID Failed to get dgraph user info by uuid err: %+v",
			err,
		)
		return
	}

	if dgraphUser == nil {
		return
	}

	err, channelPostCount := postDomain.GetLatestPostInChannelCountByUserID(ctx, userUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUserActiveChannelListWithLatestPost Failed to post count for channels err: %+v",
			err)
		return
	}

	if dgraphUser.Channels != nil {
		for ind := range dgraphUser.Channels {
			if count, ok := channelPostCount[dgraphUser.Channels[ind].Uuid]; ok {
				dgraphUser.Channels[ind].UnreadPostCount = count.PostCount
			}
		}
	}

	chatMessageCount, err := chatDomain.GetLatestChatMessageCountByUserID(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUserChatListWithLatestChat Failed to get unread chat count err: %+v",
			err)

		return
	}

	if dgraphUser.DMs != nil {
		for ind := range dgraphUser.DMs {
			if countProxy, ok := chatMessageCount[dgraphUser.DMs[ind].GroupingId]; ok && countProxy != nil {
				dgraphUser.DMs[ind].UnreadMessageCount = countProxy.ChatCount
			} else {
				dgraphUser.DMs[ind].UnreadMessageCount = 0
			}
		}
	}

	activityUnreadCount, err := lastSeenActivityBusiness.GetTotalUnreadActivityCount(ctx, dgraphUser.Uid, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphUserInfoByUUIDForSidebarNav Failed to get activity unread count err: %+v", err)
		// Don't return, just log
	} else {
		dgraphUser.TotalUnreadActivityCount = activityUnreadCount
	}

	// Active Call Hydration
	//
	// A call is "active" only when a human (non-agent) participant is in the
	// room. We can't use bare room existence here: the transcription agent
	// auto-joins every room and LiveKit keeps an empty room alive for its
	// EmptyTimeout, so existence would keep the call dot lit after everyone
	// has left. HumanActiveRoomNames filters the agent out.
	activeRoomMap, lrErr := LiveKitBusiness.HumanActiveRoomNames(ctx)
	if lrErr != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDgraphUserInfoByUUIDForSidebarNav Failed to list livekit rooms err: %+v", lrErr)
	} else {
		// Hydrate channels
		if dgraphUser.Channels != nil {
			for ind := range dgraphUser.Channels {
				if activeRoomMap[dgraphUser.Channels[ind].Uuid] {
					dgraphUser.Channels[ind].CallActive = true
				}
			}
		}

		// Hydrate DMs and Group Chats
		if dgraphUser.DMs != nil {
			for ind := range dgraphUser.DMs {
				if activeRoomMap[dgraphUser.DMs[ind].GroupingId] {
					dgraphUser.DMs[ind].CallActive = true
				}
			}
		}
	}

	return

}

func GetChannelsAndUsers(ctx context.Context, userDgraphUID string, userDgraphUUID string, searchText string) (fwdList []*adapterUser.UserAndChannelFwdMessage, err error) {
	// searchText is cleaned (dgraphquery.CleanSearchTerm), not escaped: the
	// people search escapes it itself and also matches handles in Postgres.
	safeSearch := dgraphquery.EscapeRegexLiteral(searchText)

	channelList, err := channelDomain.GetChannelListWithSearchText(ctx, userDgraphUID, safeSearch)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetChannelsAndUsers Failed to get dgraph channel list err: %+v",
			err,
		)
		return
	}

	usersList, err := domain.GetUserListWithSearchText(ctx, userDgraphUID, searchText)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetChannelsAndUsers Failed to get dgraph user list err: %+v",
			err,
		)
		return
	}

	groupChatList, err := chatDomain.GetUserGroupChatListWithSearchText(ctx, userDgraphUUID, userDgraphUID, safeSearch)
	if err != nil {
		// Non-fatal — log and continue without group chats
		helpers.LogErrorWithContext(ctx,
			"business/GetChannelsAndUsers Failed to get group chat list err: %+v",
			err,
		)
		err = nil
	}

	for _, ch := range channelList {
		fwdInfo := adapterUser.UserAndChannelFwdMessage{
			Type:             "channel",
			ChannelName:      ch.Name,
			ChannelUuid:      ch.Uuid,
			ChannelDgraphUid: ch.Uid,
		}

		fwdList = append(fwdList, &fwdInfo)
	}

	for _, u := range usersList {
		fwdInfo := adapterUser.UserAndChannelFwdMessage{
			Type:         "user",
			UserUuid:     u.Uuid,
			UserName:     u.DisplayName(),
			UserFullName: u.UserFullName,
			UserHandle:   u.Handle,
			// Not every person has a photo (invited or seeded people often
			// don't); dereferencing it unguarded crashed the forward search.
			UserProfileKey: profileKeyOrEmpty(u.ProfileKey),
			UserDgraphUid:  u.Uid,
		}

		fwdList = append(fwdList, &fwdInfo)
	}

	for _, grp := range groupChatList {
		// Build a display name from participants
		var participantNames []string
		for _, p := range grp.Participants {
			if p.Uuid != userDgraphUUID {
				participantNames = append(participantNames, p.DisplayName())
			}
		}
		grpName := "Group Chat"
		if len(participantNames) > 0 {
			grpName = strings.Join(participantNames, ", ")
		}

		fwdInfo := adapterUser.UserAndChannelFwdMessage{
			Type:         "groupChat",
			GrpId:        grp.GroupingId,
			GrpDgraphUid: grp.Uid,
			GrpName:      grpName,
		}

		fwdList = append(fwdList, &fwdInfo)
	}

	return

}

func GetDgraphUserInfoByUUIDs(ctx context.Context, userUUID []string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {
	if len(userUUID) == 0 {
		return
	}
	dgraphUsers, err = domain.GetDgraphUserInfoByUUIDs(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphUserInfoByUUIDs Failed to get dgraph user infos by uuid err: %+v",
			err,
		)
		return
	}

	return
}

func GetUsersListWhoDontBelongToTheProjectButBelongToTheTeam(ctx context.Context, teamUUID string, projectUUID string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {
	dgraphUsers, err = domain.GetUsersListWhoDontBelongToTheProjectButBelongToTheTeam(ctx, teamUUID, projectUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUsersListWhoDontBelongToTheProjectButBelongToTheTeam Failed to get users list dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetUsersListWhoDontBelongToTheTeam(ctx context.Context, teamUUID string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {
	dgraphUsers, err = domain.GetUsersListWhoDontBelongToTheTeam(ctx, teamUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUsersListWhoDontBelongToTheTeam Failed to get users list dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetUsersListWhoDontBelongToTheDM(ctx context.Context, dmUID string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {
	dgraphUsers, err = domain.GetUsersListWhoDontBelongToTheDM(ctx, dmUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUsersListWhoDontBelongToTheDM Failed to get users list dgraph err: %+v",
			err,
		)
		return
	}

	return
}

// EnsureDemoUserExists is an idempotent helper for the demo login flow.
// It checks whether the demo user already exists in postgres; if not, it
// creates them across all data stores (postgres → dgraph → opensearch),
// mirroring the OAuthCallback new-user creation path.
func EnsureDemoUserExists(ctx context.Context, emailID string, userName string) error {
	err, exists := domain.CheckIfUserExistByEmail(ctx, emailID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/EnsureDemoUserExists Failed to check user existence err: %+v", err)
		return err
	}
	if exists {
		return nil
	}

	// User doesn't exist — create across all stores
	err = CreateUser(ctx, emailID, userName)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/EnsureDemoUserExists Failed to create demo user err: %+v", err)
		return err
	}

	return nil
}

// ===== Email Auth Business Functions =====

func CreateUserWithPassword(ctx context.Context, emailID string, userName string, passwordHash *string) (err error) {
	// Default callsite: email signup. SSO flows go through CreateUserWithMethod.
	method := userModels.AuthMethodEmail
	if passwordHash == nil {
		method = "" // legacy callers (OAuth code path that hasn't been migrated)
	}
	_, _, err = CreateUserWithMethod(ctx, emailID, userName, passwordHash, method, false)
	return
}

// Joined is who JoinAsMember made a member, and where they start.
type Joined struct {
	UserID uuid.UUID
	// Landing is the channel they open on: one of the workspace's default
	// channels they were put in (WelcomeNewMember), or uuid.Nil when there is
	// none, and they start on Home as before.
	Landing uuid.UUID
	// Handle is their @handle (users.username), "" only if one could not be
	// given (logged).
	Handle string
	// Adopted reports that the account was an external row an import or a
	// GitHub sync left, now theirs, rather than a new one.
	Adopted bool
}

// welcomeNewMember puts someone who has just become a member where a new
// member starts, and returns the channel they land in. business/Channel sets
// it (WelcomeNewMembersWith): it imports this package, so this one cannot
// call it.
var welcomeNewMember func(ctx context.Context, userID uuid.UUID) uuid.UUID

// WelcomeNewMembersWith sets what becoming a member does beyond the account.
func WelcomeNewMembersWith(welcome func(ctx context.Context, userID uuid.UUID) uuid.UUID) {
	welcomeNewMember = welcome
}

// WelcomeNewMember does it for userID, returning the channel they land in, or
// uuid.Nil. JoinAsMember calls it; so does anything else that makes a member.
// It never fails a join: what it cannot do, it logs.
func WelcomeNewMember(ctx context.Context, userID uuid.UUID) uuid.UUID {
	if welcomeNewMember == nil {
		return uuid.Nil
	}
	return welcomeNewMember(ctx, userID)
}

// JoinAsMember gives emailID a member account, the way an invitation, Google or
// GitHub, or single sign-on lets someone in: by adopting the external row an
// import or GitHub sync left for that address (domain.AdoptExternalUser), when
// there is one, or by creating a member. Either way a seat is taken, and the
// new member is put in the workspace's default channels (WelcomeNewMember).
//
// Adopting keeps the row's id, so what was imported under it is theirs; a
// fresh account couldn't have the address anyway (email_id is unique). They
// keep the name the import gave them, which they can change in their profile.
//
// The address is lowercased (helpers.NormalizeEmail) and the external row
// found without regard to case, so an import that wrote "Ana@Acme.com" is
// adopted by the person who signs in as "ana@acme.com".
func JoinAsMember(
	ctx context.Context,
	emailID string,
	userName string,
	passwordHash *string,
	signupMethod string,
	isSSOManaged bool,
) (Joined, error) {
	return joinAsMember(ctx, emailID, userName, passwordHash, signupMethod, isSSOManaged, true)
}

// JoinAsStagedMember is JoinAsMember for an account a directory makes before
// the person starts (SCIM active:false): the account and its seat, adopted or
// new, without the welcome. Putting them in the default channels would tell
// those channels (user.joined) about someone who isn't there yet. They are
// welcomed when the account is activated (business/Scim SetActive) or at
// their first sign-in (business/Channel FirstSignInLanding).
func JoinAsStagedMember(
	ctx context.Context,
	emailID string,
	userName string,
	passwordHash *string,
	signupMethod string,
	isSSOManaged bool,
) (Joined, error) {
	return joinAsMember(ctx, emailID, userName, passwordHash, signupMethod, isSSOManaged, false)
}

// joinAsMember is JoinAsMember, welcoming the new member or not.
func joinAsMember(
	ctx context.Context,
	emailID string,
	userName string,
	passwordHash *string,
	signupMethod string,
	isSSOManaged bool,
	welcome bool,
) (Joined, error) {
	emailID = helpers.NormalizeEmail(emailID)
	if !helpers.AddressIsASCII(emailID) {
		return Joined{}, ErrAddressNotASCII
	}
	userID, adopted, err := domain.AdoptExternalUser(ctx, emailID, passwordHash, signupMethod, isSSOManaged)
	if err != nil {
		return Joined{}, err
	}
	var handle string
	if adopted {
		handle = ensureHandle(ctx, userID, userName, emailID)
	} else if userID, handle, err = CreateUserWithMethod(ctx, emailID, userName, passwordHash, signupMethod, isSSOManaged); err != nil {
		return Joined{}, err
	}
	// Whatever the way in, an invitation to this address is now used: the
	// admin's list says Joined, and its link no longer admits anyone.
	_ = domain.MarkInvitationJoined(ctx, emailID)
	joined := Joined{UserID: userID, Handle: handle, Adopted: adopted}
	if welcome {
		joined.Landing = WelcomeNewMember(ctx, userID)
	}
	return joined, nil
}

// CreateUserWithMethod is the canonical user-creator. It returns the new
// user's UUID and handle alongside any error, so callers (especially SSO
// callbacks) don't need to round-trip back through GetUserByEmailId to learn
// them.
//
// userName is the name they joined with: from sign-up, or whatever an
// identity provider had (a GitHub login, a directory's account name). It is
// cleaned to the person rule (helpers.CleanPersonName) and becomes the display
// name everyone sees. The handle is derived from it (helpers.HandleFromName)
// and is what has to be unique: a second Sam is @sam-2, and is still called
// Sam. This stops LDAP/SAML/OIDC JIT-provisioning from failing when two
// upstream directories happen to share a sAMAccountName / preferred_username.
func CreateUserWithMethod(
	ctx context.Context,
	emailID string,
	userName string,
	passwordHash *string,
	signupMethod string,
	isSSOManaged bool,
) (userUUID uuid.UUID, handle string, err error) {
	userUUID = uuid.New()
	emailID = helpers.NormalizeEmail(emailID)
	name := memberDisplayName(userName, emailID)

	handle, err = claimFreeHandle(ctx, helpers.HandleFromName(name, emailID), func(h string) error {
		return domain.CreateUserWithMethod(ctx, emailID, h, passwordHash, userUUID, signupMethod, isSSOManaged)
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/CreateUserWithMethod Failed to create user in postgres err: %+v", err)
		return userUUID, "", err
	}

	zeroUnixTime := time.Time{}
	dgraphUser := &dgraphStruct.DgraphUser{
		Uuid:         userUUID.String(),
		EmailID:      emailID,
		UserName:     name,
		UserFullName: name,
		Status:       dgraphStruct.USER_OPT_STATUS_ONLINE,
		DeletedAt:    &zeroUnixTime,
	}

	dgraphUID, err := CreateOrUpdateDgraphUser(ctx, dgraphUser)
	if helpers.DgraphWriteFailed(dgraphUID, err) {
		helpers.LogErrorWithContext(ctx,
			"business/CreateUserWithMethod Failed to create user in dgraph err: %+v", err)

		// The uid was previously discarded, so a mutation returning no uid and no error was
		// treated as success and this function handed back a UUID for a user absent from Dgraph.
		if err == nil {
			err = errors.New("dgraph returned no uid for the new user")
		}

		// Reverse the Postgres insert. This is the canonical creator — email signup by invitation,
		// the admin bootstrap, and every SAML/OIDC/LDAP just-in-time provision come through here —
		// so leaving the row locks that person out of their own email address permanently:
		// email_id is NOT NULL UNIQUE, user lookups read Dgraph, and for SSO every later login
		// retries provisioning and fails on the constraint again, with no self-service recovery.
		//
		// Safe only in this window: users(id) is referenced by 55 tables with a mix of CASCADE,
		// SET NULL and RESTRICT, and a user created moments ago has no rows in any of them.
		_ = helpers.CompensateOnFailure(ctx, "user row for "+emailID,
			func(undoCtx context.Context) error {
				return domain.HardDeleteUser(undoCtx, userUUID)
			})

		return userUUID, "", err
	}

	openSearchUser := &openSearchStruct.OpenSearchUser{
		Uuid:          userUUID.String(),
		UserName:      name,
		UserFullName:  name,
		UserEmail:     emailID,
		UserCreatedAt: time.Now().Unix(),
	}

	go domain.UpdateUserInOpenSearch(openSearchUser)

	return userUUID, handle, nil
}

// AddInvitationWithToken invites email with a new link, from invitedBy.
//
// An admin is told why an address can't be invited (InviteRefusal): it has an
// account here (live or deactivated), or a live invitation, whose link can be
// copied or sent again from the list. An expired invitation, or one used by
// an account that is gone, is renewed for an admin instead (renewed=true): a
// new link, and the invitation is the admin's now.
//
// A member (byAdmin false; an admin can let members invite) only invites
// addresses with no account and no invitation at all. Every other answer is
// the same refusal (memberInviteRefusal), so inviting can't be used to learn
// who has an account here, who has been deactivated, or who is invited; and
// only an admin renews an invitation.
func AddInvitationWithToken(ctx context.Context, email string, invitedBy uuid.UUID, byAdmin bool, token string, expiresAt time.Time) (renewed bool, err error) {
	email = helpers.NormalizeEmail(email)
	if err = refuseInvitingMember(ctx, email); err != nil {
		if _, refused := IsInviteRefusal(err); refused && !byAdmin {
			return false, memberInviteRefusal
		}
		return false, err
	}
	existing, err := domain.GetInvitationByEmail(ctx, email)
	if err != nil {
		return false, errors.New("failed to add invitation")
	}
	if existing != nil {
		if !byAdmin {
			return false, memberInviteRefusal
		}
		if existing.LiveAt(time.Now()) {
			return false, &InviteRefusal{Msg: fmt.Sprintf("%s is already invited and the invitation hasn't been used. Copy its link or send it again from the list.", email)}
		}
		if err = domain.UpdateInvitationTokenByID(ctx, existing.Id, token, expiresAt, invitedBy); err != nil {
			return false, errors.New("failed to add invitation")
		}
		return true, nil
	}

	err = domain.AddInvitationWithToken(ctx, email, invitedBy, token, expiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/AddInvitationWithToken Failed to add invitation err: %+v", err)
		return false, errors.New("failed to add invitation")
	}
	return false, nil
}

func GetExternalUsers(ctx context.Context, pageIndex int, pageSize int) (usersInfo []*userModels.User, hasMore bool, err error) {
	usersInfo, actualLen, err := domain.GetExternalUsers(ctx, pageIndex, pageSize)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetExternalUsers Failed to get external users err: %+v",
			err,
		)
		err = errors.New("failed to get external users")
		return
	}

	if actualLen > pageSize {
		hasMore = true
		usersInfo = usersInfo[:pageSize]
	}

	return
}

func UnlinkExternalUser(ctx context.Context, userUUID uuid.UUID) (bool, error) {
	ok, err := domain.UnlinkExternalUser(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UnlinkExternalUser Failed to unlink external user err: %+v",
			err,
		)
		return false, errors.New("failed to unlink external user")
	}
	if !ok {
		return false, errors.New("user not found or not an external user")
	}
	return true, nil
}

// profileKeyOrEmpty is a person's photo key, or "" when they have none.
func profileKeyOrEmpty(k *string) string {
	if k == nil {
		return ""
	}
	return *k
}

func GetDraphUserInfoWithProjectInfo(ctx context.Context, userUUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {

	dgraphUser, err = domain.GetDraphUserInfoWithProjectInfo(ctx, userUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetDraphUserInfoWithProjectInfo failed to get user info from dgraph err: %+v", err)
		return
	}

	return
}

func UpdateUNameByEmailID(ctx context.Context, emailID string, uName string, currentTime time.Time) (err error) {
	err = domain.UpdateUNameByEmailID(ctx, emailID, uName, currentTime)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/UpdateUNameByEmailID Failed to update user name err: %+v",
			err,
		)
		err = errors.New("failed to update user name")
		return
	}

	return
}

func GetAdminUserByUserUUIUD(ctx context.Context, userUUID uuid.UUID) (userInfo *userModels.User, err error) {
	userInfo, err = domain.GetAdminUserByUserUUID(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetAdminUserByUserUUIUD Failed to get admin userInfo  err: %+v",
			err,
		)
		err = errors.New("failed to get admin userInfo")
		return
	}

	return
}

func UpdateUserName(ctx context.Context, userUUID string, newName string) (err error) {

	currentTime := time.Now()
	dgraphUser := &dgraphStruct.DgraphUser{
		Uid:      "uid(user)",
		Uuid:     userUUID,
		UserName: newName,
	}

	_, err = domain.CreateOrUpdateDgraphUser(ctx, dgraphUser)

	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateUserName failed to update user name in dgraph err: %+v", err)
		return
	}

	openSearchUser := &openSearchStruct.OpenSearchUser{
		Uuid:          userUUID,
		UserName:      newName,
		UserFullName:  newName,
		UserUpdatedAt: currentTime.Unix(),
	}

	go domain.UpdateUserInOpenSearch(openSearchUser)

	// Fetch current user info to propagate changes across all indices
	userInfo, errFetch := domain.GetActiveDgraphUserInfoByUUID(ctx, userUUID)
	if errFetch == nil {
		profilePic := ""
		if userInfo.ProfileKey != nil {
			profilePic = *userInfo.ProfileKey
		}
		go domain.PropagateUserInfoInOpenSearch(userUUID, userInfo.UserFullName, profilePic)
	}

	return
}

func GetUserListWithSearchText(ctx context.Context, userUUID string, searchText string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {

	dgraphUsers, err = domain.GetUserListWithSearchText(ctx, userUUID, searchText)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetUserListWithSearchText Failed to get users list err: %+v",
			err)

		return
	}
	return
}

func GetDgraphUserInfoByUUIDForMQTTConfig(ctx context.Context, userUUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	dgraphUser, err = domain.GetDgraphUserInfoByUUIDForMQTTConfig(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/GetDgraphUserInfoByUUIDForMQTTConfig Failed to get dgraph user infos by uuid err: %+v",
			err,
		)
		return
	}
	return
}

func IncrementDeviceConnectedDgraphUser(ctx context.Context, userUUID string) (err error) {
	err = domain.IncrementDeviceConnectedDgraphUser(ctx, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"business/IncrementDeviceConnectedDgraphUser Failed to increment device connected by user err: %+v",
			err,
		)
		return
	}
	return
}
