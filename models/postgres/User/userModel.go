package models

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

type User struct {
	Id              uuid.UUID `gorm:"type:uuid;default:uuid_generate_v4()" json:"user_uuid,omitempty"`
	EmailID         string    `gorm:"unique" json:"user_email_id,omitempty"`
	PasswordHash    *string   `json:"-"`
	Username        *string   `json:"username,omitempty"`
	CreatedAt       time.Time `json:"user_created_at,omitempty"`
	UpdatedAt       time.Time `json:"user_updated_at,omitempty"`
	DeletedAt       time.Time `json:"user_deleted_at,omitempty"`
	IsAdmin         bool      `json:"is_admin,omitempty"`
	IsExternal      bool      `json:"is_external,omitempty"`
	IsBot           bool      `json:"is_bot,omitempty"`
	GitHubLogin     *string   `json:"github_login,omitempty"`
	GitHubAvatarURL *string   `json:"github_avatar_url,omitempty"`
	GitHubHTMLURL   *string   `json:"github_html_url,omitempty"`
	DisplayName     *string   `json:"display_name,omitempty"`

	// Auth-method tracking (migration 58). Nil-safe pointers for the strings
	// because pre-existing users have NULL until they next authenticate.
	SignupMethod    *string    `json:"signup_method,omitempty"`
	LastLoginMethod *string    `json:"last_login_method,omitempty"`
	LastLoginAt     *time.Time `json:"last_login_at,omitempty"`
	// IsSSOManaged: when true, this user MUST log in via their external IdP
	// (LDAP/SAML/OIDC). The set/change-password endpoints reject these users
	// so an admin compromise of email isn't a backdoor around the IdP.
	IsSSOManaged bool `json:"is_sso_managed,omitempty"`
}

// Auth method constants. Used in signup_method and last_login_method columns.
const (
	AuthMethodEmail  = "email"
	AuthMethodGoogle = "google"
	AuthMethodGitHub = "github"
	AuthMethodOIDC   = "oidc"
	AuthMethodSAML   = "saml"
	AuthMethodLDAP   = "ldap"
	AuthMethodDemo   = "demo"
	// AuthMethodPasskey is a sign-in with a passkey (WebAuthn).
	AuthMethodPasskey = "passkey"
	// AuthMethodSCIM records an account created by directory provisioning.
	//
	// It is a PROVISIONING method, not a login method, and that is why it is absent from IsSSOMethod
	// below: nobody signs in "via SCIM". A SCIM-created account still authenticates through SAML, OIDC
	// or LDAP, and its last_login_method records whichever one it used. Adding it to IsSSOMethod would
	// make this column's two questions — how the account came to exist, and how its owner proves who
	// they are — answer as though they were one.
	//
	// is_sso_managed is set explicitly to true by the SCIM creator instead, so a directory-provisioned
	// account can never be given a local password that bypasses the directory.
	AuthMethodSCIM = "scim"
)

// IsSSOMethod reports whether the given method is an external-IdP method.
// SSO-method users get is_sso_managed=true at signup and are blocked from
// SetPassword/ChangePassword so the local-password column stays empty.
func IsSSOMethod(m string) bool {
	switch m {
	case AuthMethodOIDC, AuthMethodSAML, AuthMethodLDAP:
		return true
	default:
		return false
	}
}

type Invitation struct {
	Id             uuid.UUID  `json:"id"`
	Email          string     `json:"email"`
	InvitedBy      uuid.UUID  `json:"invited_by"`
	Status         string     `json:"status"`
	Token          *string    `json:"token,omitempty"`
	TokenExpiresAt *time.Time `json:"token_expires_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

type UserInfo struct {
	UserPostgresInfo User                    `json:"postgres,omitempty"`
	UserDgraphInfo   dgraphStruct.DgraphUser `json:"dgraph,omitempty"`
}

type Attachment struct {
	Id        uuid.UUID `gorm:"type:uuid;default:uuid_generate_v4()"`
	ObjKey    string
	CreatedBy uuid.UUID
	CreatedAt time.Time
	DeletedAt time.Time
}

func CreateUser(query string, emailID string, userUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userUUID,
		emailID,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateUser Failed to create new user err: %+v",
			err)
		return
	}

	return
}

func CheckIfUserExistByEmail(query string, emailID string) (exist bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, emailID).Scan(&exist)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CheckIfUserExistByEmail Failed to check if user email exist err: %+v",
			err)
		return
	}

	return
}

func CheckIfUserExistByUsername(query string, uname string) (exist bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, uname).Scan(&exist)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CheckIfUserExistByUsername Failed to check if username exist err: %+v",
			err)
		return
	}

	return
}

func GetUserByUname(query string, uname *string) (user *User, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var userInfo User
	var userName sql.NullString
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, uname)
	err = row.Scan(
		&userInfo.Id,
		&userName,
		&userInfo.EmailID,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetUserByUname Failed to get user err: %+v",
			err)
		return
	}

	if userName.Valid {
		// userInfo.UserName = userName.String
	}

	if createdAt.Valid {
		userInfo.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		userInfo.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		userInfo.DeletedAt = deletedAt.Time
	}

	return &userInfo, nil
}

func GetUserByEmailId(query string, emailID *string) (user *User, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var userInfo User
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, emailID)
	err = row.Scan(
		&userInfo.Id,
		&userInfo.EmailID,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx,
			"models/GetUserByEmailId Failed to get user err: %+v",
			err)
		return
	}

	if createdAt.Valid {
		userInfo.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		userInfo.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		userInfo.DeletedAt = deletedAt.Time
	}

	return &userInfo, nil
}

func GetUserWithAdminFlagByUserUUID(query string, userUUID uuid.UUID) (user *User, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var userInfo User
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime
	var isAdmin sql.NullBool

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, userUUID)
	err = row.Scan(
		&userInfo.Id,
		&userInfo.EmailID,
		&createdAt,
		&updatedAt,
		&deletedAt,
		&isAdmin,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetUserByUname Failed to get user err: %+v",
			err)
		return
	}

	if createdAt.Valid {
		userInfo.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		userInfo.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		userInfo.DeletedAt = deletedAt.Time
	}

	if isAdmin.Valid {
		userInfo.IsAdmin = isAdmin.Bool
	}

	return &userInfo, nil
}

func GetUserByUUID(query string, uuid uuid.UUID) (user *User, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var userInfo User
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, uuid)
	err = row.Scan(
		&userInfo.Id,
		&userInfo.EmailID,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx,
			"models/GetUserByUUID Failed to get user err: %+v",
			err)
		return
	}

	if createdAt.Valid {
		userInfo.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		userInfo.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		userInfo.DeletedAt = deletedAt.Time
	}

	return &userInfo, nil

}

func UpdateUNameByEmailID(query string, uName string, currentTime time.Time, emailID string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		uName,
		currentTime,
		emailID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateUNameByEmailID Failed to update user name err: %+v",
			err)
		return
	}

	return
}

func UpdateDeletedTimeByUUID(query string, deleteTime *time.Time, updateTime *time.Time, userUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		deleteTime,
		updateTime,
		userUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateDeletedTimeByUUID Failed to update user's delete time err: %+v",
			err)
		return
	}

	return
}

func UpdateDeletedTimeToNullByUUID(query string, updateTime *time.Time, userUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		updateTime,
		userUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateDeletedTimeToNullByUUID Failed to update user's delete time to null err: %+v",
			err)
		return
	}

	return
}

func GetAdminUserByUserUUID(query string, userUUID uuid.UUID) (user *User, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var userInfo User
	var userName sql.NullString
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, userUUID)
	err = row.Scan(
		&userInfo.Id,
		&userInfo.EmailID,
		&createdAt,
		&updatedAt,
		&deletedAt,
	)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx,
			"models/GetAdminUserByUserUUID Failed to get user err: %+v",
			err)
		return
	}

	if userName.Valid {
		// userInfo.UserName = userName.String
	}

	if createdAt.Valid {
		userInfo.CreatedAt = createdAt.Time
	}

	if updatedAt.Valid {
		userInfo.UpdatedAt = updatedAt.Time
	}

	if deletedAt.Valid {
		user.DeletedAt = deletedAt.Time
	}

	return &userInfo, nil
}

func HardDeleteAdminUserByEmailId(query string, emailId string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		emailId,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/HardDeleteAdminUserByEmailId Failed to hard delete user err: %+v",
			err)
		return
	}

	return
}
func CreateAdminUser(query string, emailId string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		emailId,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateAdminUser Failed to create new admin user err: %+v",
			err)
		return
	}

	return
}

func GetAllAdminUsers(query string, args ...any) (usersInfo []*User, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetAllAdminUsers Failed to get admin users err: %+v",
			err)
		return
	}
	// A scan error inside the loop returns early, so without this the pooled
	// connection is never released — database/sql only auto-closes when Next()
	// runs to completion.
	defer rows.Close()

	// Process the rows to get the result
	for rows.Next() {
		var userInfo User
		if err = rows.Scan(&userInfo.Id, &userInfo.EmailID); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetAllAdminUsers Failed to scan row err: %+v",
				err)
			return
		}

		usersInfo = append(usersInfo, &userInfo)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without this the function returns a
	// PARTIAL result with a nil error, so the caller cannot tell truncated data
	// from a genuinely short list.
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "userModel.go rows iteration failed err: %+v", err)
		return
	}

	return
}

// GetUserIDByEmail finds a user ID by email address.
func GetUserIDByEmail(query string, email string) (uuid.UUID, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var userID uuid.UUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, email).Scan(&userID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, err
	}
	return userID, nil
}

// CreateExternalGitHubUser creates a stub user for an unmapped GitHub user.
func CreateExternalGitHubUser(query string, userUUID uuid.UUID, emailID string, login string, displayName *string, avatarURL *string, htmlURL *string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		userUUID,
		emailID,
		login,
		displayName,
		avatarURL,
		htmlURL,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateExternalGitHubUser Failed to create external user err: %+v",
			err)
		return err
	}
	return nil
}

// GetUserByGitHubLogin finds a user by their GitHub login.
func GetUserByGitHubLogin(query string, login string) (*User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var user User
	var createdAt sql.NullTime
	var updatedAt sql.NullTime
	var deletedAt sql.NullTime
	var githubLogin sql.NullString
	var githubAvatarURL sql.NullString
	var githubHTMLURL sql.NullString
	var displayName sql.NullString

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, login)
	err := row.Scan(
		&user.Id,
		&user.EmailID,
		&createdAt,
		&updatedAt,
		&deletedAt,
		&user.IsAdmin,
		&user.IsExternal,
		&githubLogin,
		&githubAvatarURL,
		&githubHTMLURL,
		&displayName,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		helpers.LogErrorWithContext(ctx,
			"models/GetUserByGitHubLogin Failed to get user err: %+v",
			err)
		return nil, err
	}

	if createdAt.Valid {
		user.CreatedAt = createdAt.Time
	}
	if updatedAt.Valid {
		user.UpdatedAt = updatedAt.Time
	}
	if deletedAt.Valid {
		user.DeletedAt = deletedAt.Time
	}
	if githubLogin.Valid {
		user.GitHubLogin = &githubLogin.String
	}
	if githubAvatarURL.Valid {
		user.GitHubAvatarURL = &githubAvatarURL.String
	}
	if githubHTMLURL.Valid {
		user.GitHubHTMLURL = &githubHTMLURL.String
	}
	if displayName.Valid {
		user.DisplayName = &displayName.String
	}

	return &user, nil
}

// GetExternalUsers returns a paginated list of external (ghost) users ordered by most recent.
func GetExternalUsers(query string, args ...any) ([]*User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetExternalUsers Failed to get external users err: %+v",
			err)
		return nil, err
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		var user User
		var createdAt sql.NullTime
		var updatedAt sql.NullTime
		var deletedAt sql.NullTime
		var githubLogin sql.NullString
		var githubAvatarURL sql.NullString
		var githubHTMLURL sql.NullString
		var displayName sql.NullString

		if err = rows.Scan(
			&user.Id,
			&user.EmailID,
			&createdAt,
			&updatedAt,
			&deletedAt,
			&user.IsAdmin,
			&user.IsExternal,
			&githubLogin,
			&githubAvatarURL,
			&githubHTMLURL,
			&displayName,
		); err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetExternalUsers Failed to scan row err: %+v",
				err)
			return nil, err
		}

		if createdAt.Valid {
			user.CreatedAt = createdAt.Time
		}
		if updatedAt.Valid {
			user.UpdatedAt = updatedAt.Time
		}
		if deletedAt.Valid {
			user.DeletedAt = deletedAt.Time
		}
		if githubLogin.Valid {
			user.GitHubLogin = &githubLogin.String
		}
		if githubAvatarURL.Valid {
			user.GitHubAvatarURL = &githubAvatarURL.String
		}
		if githubHTMLURL.Valid {
			user.GitHubHTMLURL = &githubHTMLURL.String
		}
		if displayName.Valid {
			user.DisplayName = &displayName.String
		}

		users = append(users, &user)
	}

	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetExternalUsers rows iteration err: %+v",
			err)
		return nil, err
	}

	return users, nil
}

// UnlinkExternalUser clears the GitHub linkage for an external user.
// Returns the number of rows affected (0 if user not found or not external).
func UnlinkExternalUser(userUUID uuid.UUID) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	result, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE users
		SET github_login = NULL,
			github_avatar_url = NULL,
			github_html_url = NULL,
			updated_at = NOW()
		WHERE id = $1 AND is_external = true
	`, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UnlinkExternalUser Failed to unlink user err: %+v",
			err)
		return 0, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UnlinkExternalUser Failed to get rows affected err: %+v",
			err)
		return 0, err
	}
	return rowsAffected, nil
}

// GetActiveUsersByUUIDsForNotification batches the (id, email, is_external)
// projection used by the notification dispatcher fan-out. Returns a map
// keyed by user_id; missing keys signal "user not found / deleted".
//
// Only the columns the dispatcher needs are selected to keep the
// payload small (a single notification can fan out to 50+ recipients).
// Deleted rows are excluded server-side; the caller never has to
// re-filter.
func GetActiveUsersByUUIDsForNotification(ids []uuid.UUID) (map[uuid.UUID]*User, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]*User{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	args := make([]string, len(ids))
	for i, id := range ids {
		args[i] = id.String()
	}

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx,
		`SELECT id, email_id, is_external FROM users WHERE id = ANY($1::uuid[]) AND deleted_at IS NULL`,
		"{"+strings.Join(args, ",")+"}")
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetActiveUsersByUUIDsForNotification err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	out := make(map[uuid.UUID]*User, len(ids))
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.Id, &u.EmailID, &u.IsExternal); err != nil {
			return nil, err
		}
		out[u.Id] = &u
	}
	return out, rows.Err()
}

// HardDeleteUser removes a user row outright. Compensation only.
//
// This is the most dangerous delete in the codebase and is deliberately narrow. users(id) is
// referenced by 55 tables with a MIX of behaviours — some ON DELETE CASCADE, some ON DELETE SET
// NULL, most with no clause at all and therefore RESTRICT. Run against an established user it
// would either be refused or quietly cascade into agent evals, skills and more.
//
// It exists for one moment: the row was just inserted, the matching Dgraph node could not be
// created, and nothing else has been written yet. In that window the user has no children, so
// there is nothing to cascade and nothing to restrict, and the delete restores the state that
// existed before the request.
//
// A soft delete cannot serve here. users.email_id is NOT NULL UNIQUE and users.username is
// UNIQUE, and a soft-deleted row keeps both — so the address would stay reserved by an account
// that no part of the product can see, and the person could never sign up again with their own
// email. That is the whole reason this function exists.
func HardDeleteUser(query string, userUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/HardDeleteUser Failed to delete user row err: %+v", err)
		return
	}
	return
}
