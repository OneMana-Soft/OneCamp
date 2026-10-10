package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/lib/pq"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	recordingDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Recording"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/User"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchModels "github.com/akashc777/OneCamp/models/openSearch/User"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

const BEFORE_THIRTY_DAYS = -30 * 24 * time.Hour

func CreateUser(ctx context.Context, emailID string, userUUID uuid.UUID) (err error) {
	if err = EnsureSeatAvailable(ctx); err != nil {
		return err
	}
	query := `
		INSERT INTO users (id, email_id)
		VALUES ($1, $2)
	`
	err = models.CreateUser(query, emailID, userUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateUser Failed to create new user err: %+v",
			err)
		return
	}

	return
}

// CREATE TABLE IF NOT EXISTS attachments(
//     "id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
//     "obj_key" varchar NOT NULL,
//     "created_by" uuid REFERENCES users(id),
//     "created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
//     "deleted_at" TIMESTAMP WITH TIME ZONE ,
//     CONSTRAINT unique_id_and_obj_key UNIQUE ("id", "obj_key")
// );

// CheckIfUserExistByUsername reports whether a member has this handle,
// compared without case. It asked for a column that does not exist
// (user_name), so it failed every time and answered "free".
func CheckIfUserExistByUsername(ctx context.Context, uname *string) (exist bool, err error) {
	query := `
        SELECT EXISTS (
            SELECT 1
            FROM users
            WHERE LOWER(username) = LOWER($1)
            AND is_external = false
        );
    `

	exist, err = models.CheckIfUserExistByUsername(query, *uname)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CheckIfUserExistByUsername Failed to check user by username err: %+v",
			err)
		return
	}

	return
}

func GetUserByEmailId(ctx context.Context, emailId *string) (userInfo *models.User, err error) {
	query := `
        SELECT id, email_id, created_at, updated_at, deleted_at
        FROM users
        WHERE LOWER(email_id) = LOWER($1)
		AND deleted_at IS NULL
		AND is_external = false
		` + sameAddressFirst + `
    `

	userInfo, err = models.GetUserByEmailId(query, emailId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserByEmailId Failed to get user err: %+v",
			err)
		return
	}

	return
}

// GetUserSignInFlags reads whether a person is an admin and whether single
// sign-on manages their account: what a directory sign-in needs to keep the
// admin role in step with the directory's groups.
func GetUserSignInFlags(ctx context.Context, userID uuid.UUID) (isAdmin, isSSOManaged bool, err error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	err = postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		SELECT (au.id IS NOT NULL), COALESCE(u.is_sso_managed, false)
		FROM users u
		LEFT JOIN admin_users au ON au.email_id = u.email_id
		WHERE u.id = $1
		LIMIT 1`, userID).Scan(&isAdmin, &isSSOManaged)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetUserSignInFlags err: %+v", err)
	}
	return
}

// FirstSignInOfProvisioned reports whether this person's account was made
// for them by the directory (SCIM) and they have never signed in: their
// first sign-in is when they arrive, as joining is for everyone else. A read
// failure reads as no.
func FirstSignInOfProvisioned(ctx context.Context, userID uuid.UUID) bool {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var first bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		SELECT COALESCE(signup_method, '') = $2 AND last_login_at IS NULL
		FROM users WHERE id = $1`, userID, models.AuthMethodSCIM).Scan(&first)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/FirstSignInOfProvisioned err: %+v", err)
		return false
	}
	return first
}

// searchTextMaxLen bounds the needle. A regex is built from it, and an
// arbitrarily long one is a way to make the store work very hard for nothing.
const searchTextMaxLen = 100

// escapeForDgraphRegex makes a caller's text safe to interpolate into a dgraph
// regexp literal.
//
// WHY THIS IS NEEDED. The needle is not always typed by a person into a picker.
// One caller resolves an @name the MODEL produced, and the result is used as a
// DM recipient and as a task assignee; another resolves a name for sharing. So
// the text can be influenced by whatever the model just read, which is workspace
// content anybody can author.
//
// Unescaped it was interpolated straight into /.*TEXT.*/i. Regex metacharacters
// alone let a crafted name widen the match, which matters when the caller then
// takes found[0] and sends somebody a direct message. A "/" is worse: it closes
// the literal, so the rest of the needle lands in the filter expression itself.
//
// QuoteMeta handles the regex; the slash has to be done separately because
// QuoteMeta has no idea the pattern is about to be wrapped in delimiters.
func escapeForDgraphRegex(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > searchTextMaxLen {
		s = s[:searchTextMaxLen]
	}
	return strings.ReplaceAll(regexp.QuoteMeta(s), "/", `\/`)
}

// GetUserListWithSearchText is the members whose display name, full name or
// @handle contains searchText, without case; a leading @ is the handle's.
// Each comes with their handle. searchText is as typed (cleaned, not
// escaped): it is escaped here for Dgraph and matched as text in Postgres,
// where handles are.
//
// It used to look at the display name alone, so nobody was found by their
// full name or their handle.
func GetUserListWithSearchText(ctx context.Context, userUUID string, searchText string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {
	searchText = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(searchText), "@"))
	if searchText == "" {
		return nil, nil
	}
	variables := make(map[string]string)
	variables["$user_id"] = userUUID
	// Escaped HERE rather than in each caller: there are several, one of them
	// feeds a DM recipient, and a caller that forgets is silent.
	safeSearch := escapeForDgraphRegex(searchText)
	match := fmt.Sprintf(`regexp(user_name, /.*%[1]s.*/i) OR regexp(user_full_name, /.*%[1]s.*/i)`, safeSearch)
	// Handles are one more way to be found: when Postgres can't say whose
	// match, the names still find people, rather than nobody being found.
	byHandle, herr := MemberIDsWithHandleLike(ctx, searchText, userSearchHandleLimit)
	if herr != nil {
		helpers.LogWarnWithContext(ctx, "domain/GetUserListWithSearchText searching names only, handles unread: %+v", herr)
	}
	if len(byHandle) > 0 {
		quoted := make([]string, len(byHandle))
		for i, id := range byHandle {
			quoted[i] = `"` + id.String() + `"`
		}
		match += ` OR eq(user_uuid, [` + strings.Join(quoted, ", ") + `])`
	}
	query := fmt.Sprintf(`query UserInfo($user_id: string){
			userInfo(func: has(user_uuid)) @filter((%s) AND NOT eq(is_external, true)) {
				uid
				user_uuid
				user_name
				user_full_name
				user_profile_object_key
				user_device_connected
				user_email_id
				user_status
		  	}
			}`, match)

	dgraphUsers, err = dgraphModels.GetDgraphUsersList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserListWithSearchText Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}
	AttachHandles(ctx, dgraphUsers)
	return
}

// userSearchHandleLimit bounds the members a people search finds by handle.
const userSearchHandleLimit = 50

// MemberIDsWithHandleLike is up to limit members (not external rows, bots or
// deleted accounts) whose handle contains term, without case.
func MemberIDsWithHandleLike(ctx context.Context, term string, limit int) ([]uuid.UUID, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	escaped := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.ToLower(term))
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT id FROM users
		WHERE LOWER(username) LIKE $1 ESCAPE '\'
		  AND is_external = false AND is_bot = false AND deleted_at IS NULL
		ORDER BY username LIMIT $2`, "%"+escaped+"%", limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MemberIDsWithHandleLike err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// HandlesByID is the handle of each of these people who has one, by id.
func HandlesByID(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT id::text, username FROM users
		WHERE id = ANY($1::uuid[]) AND username IS NOT NULL AND username <> ''`, pq.Array(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/HandlesByID err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, handle string
		if err := rows.Scan(&id, &handle); err != nil {
			return nil, err
		}
		out[id] = handle
	}
	return out, rows.Err()
}

// AttachHandles sets each user's Handle from Postgres, where handles are, so
// a list of people can be searched and shown by @handle. Best effort: on a
// failure the list goes without them.
func AttachHandles(ctx context.Context, users []*dgraphStruct.DgraphUser) {
	ids := make([]string, 0, len(users))
	for _, u := range users {
		if u != nil && u.Uuid != "" {
			ids = append(ids, u.Uuid)
		}
	}
	handles, err := HandlesByID(ctx, ids)
	if err != nil {
		return
	}
	for _, u := range users {
		if u != nil {
			u.Handle = handles[u.Uuid]
		}
	}
}

func GetActiveUserWithAdminFlagByUserUUID(ctx context.Context, userUUID uuid.UUID) (user *models.User, err error) {
	found, _ := redisStore.GetJSON(ctx, registry.UserProfile, []string{userUUID.String()}, &user)
	if found {
		return
	}

	query := `
        SELECT
			u.id,
			u.email_id,
			u.created_at,
			u.updated_at,
			u.deleted_at,
			(CASE WHEN au.id IS NOT NULL THEN true ELSE false END) AS is_admin,
			u.is_external,
			u.is_bot
		FROM users u
		LEFT JOIN admin_users au ON u.email_id = au.email_id
		WHERE u.id = $1
		AND u.deleted_at IS NULL;
    `

	user, err = models.GetUserWithAdminFlagByUserUUID(query, userUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetActiveUserWithAdminFlagByUserUUID Failed to get user info from postgres err: %+v",
			err)
		return
	}

	if user != nil {
		_ = redisStore.SetJSON(ctx, registry.UserProfile, []string{userUUID.String()}, user)
	}

	return
}

func GetActiveUserByUUID(ctx context.Context, uuid uuid.UUID) (userInfo *models.User, err error) {
	query := `
        SELECT id, email_id, created_at, updated_at, deleted_at, is_external, is_bot
        FROM users
        WHERE id = $1
		AND deleted_at IS NULL
    `

	userInfo, err = models.GetUserByUUID(query, uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserByEmailId Failed to get user err: %+v",
			err)
		return
	}

	return
}

func GetUserByUUID(ctx context.Context, uuid uuid.UUID) (userInfo *models.User, err error) {
	query := `
        SELECT id, email_id, created_at, updated_at, deleted_at, is_external, is_bot
        FROM users
        WHERE id = $1
    `

	userInfo, err = models.GetUserByUUID(query, uuid)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserByEmailId Failed to get user err: %+v",
			err)
		return
	}

	return
}

func UpdateDeletedTimeToNullByUUID(ctx context.Context, updateTime *time.Time, userUUID uuid.UUID) (err error) {
	if err = ensureSeatForReactivation(ctx, userUUID); err != nil {
		return err
	}
	query := `
        UPDATE users
        SET deleted_at = null, updated_at = $1
		WHERE id = $2
    `

	err = models.UpdateDeletedTimeToNullByUUID(query, updateTime, userUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateDeletedTimeToNullByUUID Failed to update user's delete time name err: %+v",
			err)
		return
	}

	return
}

func UpdateDeletedTimeByUUID(ctx context.Context, deleteTime *time.Time, updateTime *time.Time, userUUID uuid.UUID) (err error) {
	query := `
        UPDATE users
        SET deleted_at = $1, updated_at = $2
		WHERE id = $3
    `

	err = models.UpdateDeletedTimeByUUID(query, deleteTime, updateTime, userUUID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateDeletedTimeByUUID Failed to update user's delete time name err: %+v",
			err)
		return
	}

	return
}

func GetAllAdminUsers(ctx context.Context, pageIndex int, pageSize int) (usersInfo []*models.User, actualLen int, err error) {
	offset := pageIndex * pageSize
	query := `
        SELECT 
		u.id AS user_id,
		u.email_id
		FROM 
			users u
		JOIN 
			admin_users a 
		ON 
			u.email_id = a.email_id
		LIMIT $1 OFFSET $2
    `

	usersInfo, err = models.GetAllAdminUsers(query, pageSize+1, offset)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetAllAdminUsers Failed to get user err: %+v",
			err)
		return
	}

	actualLen = len(usersInfo)
	return
}

func HardDeleteAdminUserByEmailId(ctx context.Context, emailId string, userUUID ...string) (err error) {
	query := `
		DELETE FROM admin_users
        WHERE LOWER(email_id) = LOWER($1)
	`
	err = models.HardDeleteAdminUserByEmailId(query, emailId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/HardDeleteAdminUserByEmailId Failed to delete admin user err: %+v",
			err)
		return
	}

	// Invalidate the cache for the demoted user
	var targetUUID string
	if len(userUUID) > 0 && userUUID[0] != "" {
		targetUUID = userUUID[0]
	} else {
		if user, getErr := GetUserByEmailId(ctx, &emailId); getErr == nil && user != nil {
			targetUUID = user.Id.String()
		}
	}

	if targetUUID != "" {
		_ = redisStore.Delete(ctx, registry.UserProfile, []string{targetUUID})
		_ = redisStore.Delete(ctx, registry.UserDgraphProfile, []string{targetUUID})
	}

	return
}

func CreateAdminUser(ctx context.Context, emailId string, userUUID ...string) (err error) {
	query := `
		INSERT INTO admin_users (email_id)
        VALUES (COALESCE(
			(SELECT email_id FROM users WHERE LOWER(email_id) = LOWER($1) AND is_external = false AND is_bot = false ` + sameAddressFirst + `),
			LOWER($1)))
        ON CONFLICT (email_id) DO NOTHING;
	`
	err = models.CreateAdminUser(query, emailId)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateAdminUser Failed to create admin user err: %+v",
			err)
		return
	}

	// Invalidate the cache for the promoted user
	var targetUUID string
	if len(userUUID) > 0 && userUUID[0] != "" {
		targetUUID = userUUID[0]
	} else {
		if user, getErr := GetUserByEmailId(ctx, &emailId); getErr == nil && user != nil {
			targetUUID = user.Id.String()
		}
	}

	if targetUUID != "" {
		_ = redisStore.Delete(ctx, registry.UserProfile, []string{targetUUID})
		_ = redisStore.Delete(ctx, registry.UserDgraphProfile, []string{targetUUID})
	}

	return
}

// sameAddressFirst picks one account when a lookup by address finds more
// than one: an install from before addresses were lowercased can hold two
// whose addresses differ only in capital letters. The one spelt exactly as
// asked comes first, then the older one. Callers pass a NormalizeEmail'd
// address, so for a sign-in that is the all-lowercase account.
// The admin's system check names such addresses (AddressesWithMoreThanOneAccount).
const sameAddressFirst = `ORDER BY (email_id = $1) DESC, created_at ASC LIMIT 1`

// AddressesWithMoreThanOneAccount lists up to limit addresses that more than
// one member's account has, compared without case.
func AddressesWithMoreThanOneAccount(ctx context.Context, limit int) ([]string, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT LOWER(email_id) FROM users
		WHERE is_external = false AND is_bot = false
		GROUP BY LOWER(email_id) HAVING count(*) > 1
		ORDER BY 1 LIMIT $1`, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/AddressesWithMoreThanOneAccount err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		out = append(out, email)
	}
	return out, rows.Err()
}

// TakenHandles is every handle in use that a new one based on base could
// collide with: base itself and base-<anything>, compared without case.
func TakenHandles(ctx context.Context, base string) ([]string, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	escaped := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.ToLower(base))
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT LOWER(username) FROM users
		WHERE LOWER(username) = LOWER($1) OR LOWER(username) LIKE $2 ESCAPE '\'`, base, escaped+"-%")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/TakenHandles err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var taken []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		taken = append(taken, h)
	}
	return taken, rows.Err()
}

// GetHandle is a person's handle (users.username), or "" when they have none.
func GetHandle(ctx context.Context, userID uuid.UUID) (string, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var handle sql.NullString
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `SELECT username FROM users WHERE id = $1`, userID).Scan(&handle)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetHandle err: %+v", err)
		return "", err
	}
	return handle.String, nil
}

// MemberWithoutHandle is a member who has no @handle yet, and when they
// joined.
type MemberWithoutHandle struct {
	ID       uuid.UUID
	Email    string
	JoinedAt time.Time
}

// MembersWithoutHandle is up to limit members who have no handle (people,
// not an external row, a bot or a deleted account), in the order they
// joined and then by id, after the member after; its zero value comes before
// everyone. Accounts from before handles (v2.70.0) have none.
//
// In join order, so that of two people called Sam the one who joined first
// is @sam: in id order the later one could be. A join time never recorded
// counts as the earliest.
func MembersWithoutHandle(ctx context.Context, after MemberWithoutHandle, limit int) ([]MemberWithoutHandle, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT id, email_id, COALESCE(created_at, 'epoch') AS joined FROM users
		WHERE (username IS NULL OR username = '')
		  AND is_external = false AND is_bot = false AND deleted_at IS NULL
		  AND (COALESCE(created_at, 'epoch'), id) > ($1, $2)
		ORDER BY joined, id LIMIT $3`, after.JoinedAt, after.ID, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MembersWithoutHandle err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []MemberWithoutHandle
	for rows.Next() {
		var m MemberWithoutHandle
		if err := rows.Scan(&m.ID, &m.Email, &m.JoinedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// HandleTakenByAnother reports whether someone other than userID has this
// handle, compared without case.
func HandleTakenByAnother(ctx context.Context, handle string, userID uuid.UUID) (bool, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var taken bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		SELECT EXISTS (SELECT 1 FROM users WHERE LOWER(username) = LOWER($1) AND id <> $2)`, handle, userID).Scan(&taken)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/HandleTakenByAnother err: %+v", err)
	}
	return taken, err
}

// SetHandle changes someone's handle. A unique violation means another person
// has it.
func SetHandle(ctx context.Context, userID uuid.UUID, handle string) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c,
		`UPDATE users SET username = $2, updated_at = NOW() WHERE id = $1`, userID, handle)
	return err
}

// SetHandleIfMissing gives someone without a handle this one, and reports
// whether it did. A unique violation means another person took it first.
func SetHandleIfMissing(ctx context.Context, userID uuid.UUID, handle string) (bool, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		UPDATE users SET username = $2, updated_at = NOW()
		WHERE id = $1 AND (username IS NULL OR username = '')`, userID, handle)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ExternalName is the name an import or a GitHub sync gave the person at
// this address, on the external row it left for them, or "" for none.
func ExternalName(ctx context.Context, email string) (string, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var name sql.NullString
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		SELECT COALESCE(NULLIF(display_name, ''), username) FROM users
		WHERE LOWER(email_id) = LOWER($1) AND is_external = true AND is_bot = false AND deleted_at IS NULL
		`+sameAddressFirst, email).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/ExternalName err: %+v", err)
		return "", err
	}
	return name.String, nil
}

// CheckIfUserExistByEmail reports whether an account, LIVE OR DEACTIVATED, has
// this address: what signing in and signing up ask, since a deactivated
// account still owns its address. It is not a live-member check: a
// deactivated account answers true here, and MemberAccountState says which
// it is. The external row an import or GitHub sync made for someone's address
// isn't their account, and neither is a bot's: it counted as one, so its
// owner signed in through Google or GitHub without an invitation, either way
// as an external who never took a seat. JoinAsMember adopts such a row
// instead.
func CheckIfUserExistByEmail(ctx context.Context, emailID string) (err error, exist bool) {
	query := `
		SELECT EXISTS
		(SELECT 1 FROM users WHERE LOWER(email_id) = LOWER($1) AND is_external = false AND is_bot = false);
	`

	exist, err = models.CheckIfUserExistByEmail(query, emailID)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CheckIfUserExistByEmail Failed to check if user exist err: %+v",
			err)
		return
	}

	return
}

func UpdateUserInOpenSearch(openSearchUser *openSearchStruct.OpenSearchUser) {
	ctx := context.Background()
	err := OpenSearchModels.UpdateUserInOpenSearch(ctx, openSearchUser)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateUserInOpenSearch Failed to update user in opensearch err: %+v",
			err)
		return
	}
}

func GetAllInvitations(ctx context.Context) (invitations []*models.Invitation, err error) {

	query := `
		SELECT id, email, invited_by, status, token, token_expires_at, created_at
		FROM invitations
		ORDER BY created_at DESC
	`
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetAllInvitations Failed to get invitations err: %+v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var inv models.Invitation
		if err = rows.Scan(&inv.Id, &inv.Email, &inv.InvitedBy, &inv.Status, &inv.Token, &inv.TokenExpiresAt, &inv.CreatedAt); err != nil {
			helpers.LogErrorWithContext(ctx, "domain/GetAllInvitations Failed to scan invitation err: %+v", err)
			return
		}
		invitations = append(invitations, &inv)
	}
	return
}

func DeleteInvitationByEmail(ctx context.Context, email string) (err error) {
	query := `
		DELETE FROM invitations
		WHERE LOWER(email) = LOWER($1)
	`
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, email)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DeleteInvitationByEmail Failed to delete invitation err: %+v", err)
		return
	}
	return
}

func PropagateUserInfoInOpenSearch(userUUID string, newFullName string, newProfilePic string) {

	ctx := context.Background()
	err := OpenSearchModels.PropagateUserInfoChangeInOpenSearch(ctx, userUUID, newFullName, newProfilePic)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/PropagateUserInfoInOpenSearch Failed to propagate user info in opensearch err: %+v",
			err)
		return
	}
}

func GetUsersListWhoDontBelongToTheTeam(ctx context.Context, teamUUID string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {
	variables := make(map[string]string)
	variables["$id"] = teamUUID

	query := `query UserInfo($id: string){
		var(func: eq(team_uuid, $id)) {
			team_m as team_members
		}
		userInfo(func: has(user_uuid)) @filter(not uid(team_m) AND NOT eq(is_external, true)) {
			uid
			user_uuid
			user_name
			user_email_id
			user_deleted_at
			user_app_lang
			user_profile_object_key
		}

	}`

	dgraphUsers, err = dgraphModels.GetDgraphUsersList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUsersListWhoDontBelongToTheTeam Failed to get user's task list from dgraph err: %+v",
			err,
		)
		return
	}
	return
}

func GetUsersListWhoDontBelongToTheDM(ctx context.Context, dmUID string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {
	variables := make(map[string]string)
	variables["$id"] = dmUID

	query := `query UserInfo($id: string){
		var(func: uid($id)) {
			dm_p as dm_participants
		}
		userInfo(func: has(user_uuid)) @filter(not uid(dm_p) AND (NOT eq(is_external, true) OR eq(is_bot, true))) {
			uid
			user_uuid
			user_name
			user_email_id
			user_deleted_at
			user_app_lang
			user_profile_object_key
			is_bot
		}

	}`

	dgraphUsers, err = dgraphModels.GetDgraphUsersList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUsersListWhoDontBelongToTheDM Failed to get user's task list from dgraph err: %+v",
			err,
		)
		return
	}
	return
}

func GetUsersListWhoDontBelongToTheProjectButBelongToTheTeam(ctx context.Context, teamUUID string, projectUUID string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {
	variables := make(map[string]string)
	variables["$teamUUID"] = teamUUID
	variables["$projectUUID"] = projectUUID

	query := `query UserInfo($teamUUID: string, $projectUUID: string){
		var(func: eq(team_uuid, $teamUUID)) {
			t as uid
		}
		var(func: eq(project_uuid, $projectUUID)) {
			p as uid
		}
		userInfo(func: has(user_uuid)) @filter(not uid_in(~project_members, uid(p)) AND uid_in(~team_members, uid(t)) AND NOT eq(is_external, true)) {
			uid
			user_uuid
			user_name
			user_email_id
			user_deleted_at
			user_app_lang
			user_profile_object_key
		}

	}`

	dgraphUsers, err = dgraphModels.GetDgraphUsersList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUsersListWhoDontBelongToTheProjectButBelongToTheTeam Failed to get user's task list from dgraph err: %+v",
			err,
		)
		return
	}
	return
}

func UpdateDgraphUserStatus(ctx context.Context, dgraphUserStatus *dgraphStruct.DgraphUserStatusEmoji, userUUID string) (err error) {
	dgraphUserStatus.DType = []string{"Status"}
	err = dgraphModels.UpdateUserEmojiStatus(ctx, dgraphUserStatus)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateDgraphUserStatus Failed to update status in dgraph err: %+v",
			err,
		)
		return
	}

	// Invalidate cache
	_ = redisStore.Delete(ctx, registry.UserDgraphProfile, []string{userUUID})

	return
}

func CreateOrUpdateDgraphUser(ctx context.Context, dgraphUser *dgraphStruct.DgraphUser) (newUserUid string, err error) {

	dgraphUser.DType = []string{"User"}
	dgraphUser.Uid = "uid(user)"

	query := fmt.Sprintf(`query {
									  user as var(func: eq(user_uuid, "%+v"))
								  }`, dgraphUser.Uuid)

	newUserUid, err = dgraphModels.CreateOrUpdateDgraphUser(ctx, dgraphUser, query, "")

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphUser Failed to create user in dgraph err: %+v",
			err,
		)
		return
	}

	// Invalidate cache
	_ = redisStore.Delete(ctx, registry.UserProfile, []string{dgraphUser.Uuid})
	_ = redisStore.Delete(ctx, registry.UserDgraphProfile, []string{dgraphUser.Uuid})

	return
}

func GetUserPostsByUserUUID(ctx context.Context, userUUID string, userDgraphUID string, pageIndex int, pageSize int) (dgraphUser *dgraphStruct.DgraphUser, err error) {

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize + 1)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$user_id"] = userUUID
	variables["$first"] = firstVal
	variables["$offset"] = offsetVal
	variables["$user_uid"] = userDgraphUID

	query := `query UserInfo($user_id: string, $user_uid: string, $first: int, $offset: int){
				userInfo(func: eq(user_uuid, $user_id))  {
					user_posts @filter(not gt(post_deleted_at, "1970-01-01T00:00:00Z")) (orderdesc: post_created_at, first: $first, offset: $offset) @cascade(post_channel){
						post_uuid
						post_text
						post_created_at
						post_by {
							user_uuid
							user_name
							user_profile_object_key
							user_full_name
						}
						post_comment_count : count(post_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						post_channel @filter(uid_in(ch_members, $user_uid)) {
							ch_name
							ch_uuid
						}
						post_reactions {
							uid
							
						}
					}
				}

			}`

	dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserPostsByUserUUID Failed to get user's post in dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetActiveDgraphUserInfoByUUID(ctx context.Context, userUUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	found, _ := redisStore.GetJSON(ctx, registry.UserDgraphProfile, []string{userUUID}, &dgraphUser)
	if found && dgraphUser != nil {
		return
	}

	// @filter(gt(status_user_emoji_expiry_at, $time))
	variables := make(map[string]string)
	variables["$id"] = userUUID
	variables["$time"] = time.Now().Format(time.RFC3339Nano)

	query := `query UserInfo($id: string, $time: string){
				# Tasks of archived projects are not the user's work any more.
				var(func: eq(user_uuid, $id)) {
					user_projects @filter(gt(project_deleted_at, "1970-01-01T00:00:00Z")) { archivedProjects as uid }
				}
				userInfo(func: eq(user_uuid, $id)) @filter(not gt(user_deleted_at, "1970-01-01T00:00:00Z")) {
					uid
					user_uuid
					user_name
					user_full_name
					user_email_id
					user_posts
					user_profile_object_key
					user_job_title
					user_hobbies
					user_device_connected
					user_status
					user_emoji_statuses @filter(gt(status_user_emoji_expiry_at, $time)) (orderdesc: status_user_emoji_expiry_at, first: 1) {
						uid
						status_user_emoji_id
						status_user_emoji_desc
						status_user_emoji_expiry_at
						status_user_emoji_expiry_in
					}
					user_projects {
						project_uuid
						project_name
						uid
					}
					user_channels {
						ch_uuid
						ch_name
						uid
					}
					user_teams {
						team_uuid
						team_name
						uid
						team_projects {
							project_uuid
						}
					}
				user_dms {
					dm_grouping_id
					dm_participants {
						user_uuid
						user_name
						user_full_name
						is_bot
						user_profile_object_key
					}
				}
			user_task_count: count(user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects))))
			user_incomplete_task_count: count(user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) AND ` + dgraphStruct.TASK_OPEN_FILTER + ` AND not eq(task_status, "canceled")))
			user_overdue_task_count: count(user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) AND ` + dgraphStruct.TASK_OPEN_FILTER + ` AND not eq(task_status, "canceled") AND lt(task_due_date, $time) AND gt(task_due_date, "1970-01-01T00:00:00Z")))
			user_created_at
			user_updated_at
			user_deleted_at
			user_app_lang
			user_theme_color
			user_theme_mode
		}
	}`

	dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUID Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	zero := 0
	if dgraphUser != nil && dgraphUser.Status == dgraphStruct.USER_OPT_STATUS_OFFLINE {
		dgraphUser.DevicesConnected = &zero
	}

	if dgraphUser != nil {
		_ = redisStore.SetJSON(ctx, registry.UserDgraphProfile, []string{userUUID}, dgraphUser)
	}

	return
}

// InvalidateUserMemberships drops a person's cached graph profile after a
// write that took a channel, project or team from them. The profile
// (user:dgraph, kept an hour) lists their channels, projects and teams; every
// request reads it through the auth middleware, and search, the AI's reach and
// command delivery are scoped by it. Until it's dropped, someone removed from
// a private channel keeps finding what's posted there.
func InvalidateUserMemberships(ctx context.Context, userUUID string) {
	if userUUID == "" {
		return
	}
	_ = redisStore.Delete(ctx, registry.UserDgraphProfile, []string{userUUID})
}

func GetAllUserEmojiStatusList(ctx context.Context, userUUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {

	beforeThirtyDaysTime := time.Now().UTC().Add(BEFORE_THIRTY_DAYS)

	variables := make(map[string]string)
	variables["$id"] = userUUID
	variables["$time"] = beforeThirtyDaysTime.Format(time.RFC3339Nano)

	query := `query UserInfo($id: string, $time: string){
				userInfo(func: eq(user_uuid, $id)) {
					uid
					user_uuid
					user_emoji_statuses @filter(gt(status_user_emoji_expiry_at, $time)) (orderdesc: status_user_emoji_expiry_at) {
						status_user_emoji_id
						status_user_emoji_desc
						status_user_emoji_expiry_at
						status_user_emoji_expiry_in
					}
				}
			}`

	dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUIDForSidebarNav Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphUserTaskListForKanban(ctx context.Context, userUUID string, userDgraphUID string, filterQuery string, closedLimit int) (dgraphUser *dgraphStruct.DgraphUser, err error) {

	if len(filterQuery) > 0 {
		filterQuery = "AND " + filterQuery
	}
	variables := make(map[string]string)
	variables["$id"] = userUUID
	variables["$userDgraphUID"] = userDgraphUID

	query := fmt.Sprintf(`query UserInfo($id: string, $userDgraphUID: string){
				# Tasks of archived projects are not the user's work any more.
				var(func: eq(user_uuid, $id)) {
					user_projects @filter(gt(project_deleted_at, "1970-01-01T00:00:00Z")) { archivedProjects as uid }
				}
				userInfo(func: eq(user_uuid, $id)) {
					user_uuid
					user_tasks_todo: user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) AND eq(task_status, "todo") %s) (orderdesc: task_created_at) {
						task_uuid
						id: task_uuid
						task_name
						task_status
						task_custom_status
						task_custom_status_name
						task_due_date
						task_start_date
						task_label
						task_description
						task_priority
						task_project {
							uid
							project_uuid
							project_name
							project_is_admin: count(project_admins @filter(uid($userDgraphUID)))
						}
						task_assignee {
							uid
							user_uuid
							user_name
							user_profile_object_key
						}
						task_sub_task_count: count(task_sub_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")))
						task_comment_count: count(task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						`+dgraphStruct.TASK_BLOCKED_OPEN+`
						task_team {
							team_name
							team_uuid
						}
						task_created_at
						task_rank
						task_status_since
					}
					user_tasks_in_progress: user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) AND eq(task_status, "inProgress") %s) (orderdesc: task_created_at) {
						task_uuid
						id: task_uuid
						task_name
						task_status
						task_custom_status
						task_custom_status_name
						task_due_date
						task_start_date
						task_label
						task_description
						task_priority
						task_project {
							uid
							project_uuid
							project_name
							project_is_admin: count(project_admins @filter(uid($userDgraphUID)))
						}
						task_assignee {
							uid
							user_uuid
							user_name
							user_profile_object_key
						}
						task_sub_task_count: count(task_sub_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")))
						task_comment_count: count(task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						`+dgraphStruct.TASK_BLOCKED_OPEN+`
						task_team {
							team_name
							team_uuid
						}
						task_created_at
						task_rank
						task_status_since
					}
					user_tasks_backlog: user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) AND eq(task_status, "backlog") %s) (orderdesc: task_created_at) {
						task_uuid
						id: task_uuid
						task_name
						task_status
						task_custom_status
						task_custom_status_name
						task_due_date
						task_start_date
						task_label
						task_description
						task_priority
						task_project {
							uid
							project_uuid
							project_name
							project_is_admin: count(project_admins @filter(uid($userDgraphUID)))
						}
						task_assignee {
							uid
							user_uuid
							user_name
							user_profile_object_key
						}
						task_sub_task_count: count(task_sub_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")))
						task_comment_count: count(task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						`+dgraphStruct.TASK_BLOCKED_OPEN+`
						task_team {
							team_name
							team_uuid
						}
						task_created_at
						task_rank
						task_status_since
					}
					user_tasks_in_review: user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) AND eq(task_status, "inReview") %s) (orderdesc: task_created_at) {
						task_uuid
						id: task_uuid
						task_name
						task_status
						task_custom_status
						task_custom_status_name
						task_due_date
						task_start_date
						task_label
						task_description
						task_priority
						task_project {
							uid
							project_uuid
							project_name
							project_is_admin: count(project_admins @filter(uid($userDgraphUID)))
						}
						task_assignee {
							uid
							user_uuid
							user_name
							user_profile_object_key
						}
						task_sub_task_count: count(task_sub_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")))
						task_comment_count: count(task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						`+dgraphStruct.TASK_BLOCKED_OPEN+`
						task_team {
							team_name
							team_uuid
						}
						task_created_at
						task_rank
						task_status_since
					}
					user_tasks_canceled: user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) AND eq(task_status, "canceled") %s) (orderdesc: task_created_at%s) {
						task_uuid
						id: task_uuid
						task_name
						task_status
						task_custom_status
						task_custom_status_name
						task_due_date
						task_start_date
						task_label
						task_description
						task_priority
						task_project {
							uid
							project_uuid
							project_name
							project_is_admin: count(project_admins @filter(uid($userDgraphUID)))
						}
						task_assignee {
							uid
							user_uuid
							user_name
							user_profile_object_key
						}
						task_sub_task_count: count(task_sub_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")))
						task_comment_count: count(task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						`+dgraphStruct.TASK_BLOCKED_OPEN+`
						task_team {
							team_name
							team_uuid
						}
						task_created_at
						task_rank
						task_status_since
					}
					user_tasks_canceled_count: count(user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) AND eq(task_status, "canceled") %s))

					user_tasks_done: user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) AND eq(task_status, "done") %s) (orderdesc: task_created_at%s) {
						task_uuid
						id:task_uuid
						task_name
						task_status
						task_custom_status
						task_custom_status_name
						task_due_date
						task_start_date
						task_label
						task_description
						task_priority
						task_project {
							uid
							project_uuid
							project_name
							project_is_admin: count(project_admins @filter(uid($userDgraphUID)))
						}
						task_assignee {
							uid
							user_uuid
							user_name
							user_profile_object_key
						}
						task_sub_task_count: count(task_sub_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")))
						task_comment_count: count(task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						`+dgraphStruct.TASK_BLOCKED_OPEN+`
						task_team {
							team_name
							team_uuid
						}
						task_created_at
						task_rank
						task_status_since
					}
					user_tasks_done_count: count(user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) AND eq(task_status, "done") %s))
				}
			}`, filterQuery, filterQuery, filterQuery, filterQuery, filterQuery, dgraphStruct.ClosedFirst(closedLimit), filterQuery, filterQuery, dgraphStruct.ClosedFirst(closedLimit), filterQuery)
	dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserTaskListForKanban Failed to get user's task list from dgraph err: %+v",
			err,
		)
		return
	}
	return

}

func GetDgraphUserTaskList(ctx context.Context, userUUID string, userDgraphUID string, filterQuery string, sortQuery string, pageSize int, pageIndex int, getAll bool) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	offset := pageIndex * pageSize

	if len(filterQuery) > 0 {
		filterQuery = "AND " + filterQuery
	}
	variables := make(map[string]string)
	variables["$id"] = userUUID
	variables["$userDgraphUID"] = userDgraphUID
	firstVal := strconv.Itoa(pageSize)
	offsetVal := strconv.Itoa(offset)

	if !getAll {
		sortQuery = fmt.Sprintf("first: %v, offset: %v ,", firstVal, offsetVal) + sortQuery
	}

	query := fmt.Sprintf(`query UserInfo($id: string, $userDgraphUID: string){
				# Tasks of archived projects are not the user's work any more.
				var(func: eq(user_uuid, $id)) {
					user_projects @filter(gt(project_deleted_at, "1970-01-01T00:00:00Z")) { archivedProjects as uid }
				}
				userInfo(func: eq(user_uuid, $id)) {
					user_task_count: count(user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) %s))
					user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) %s) ( %s) {
						task_uuid
						task_name
						task_status
						task_custom_status
						task_custom_status_name
						task_due_date
						task_start_date
						task_label
						task_description
						task_priority
						task_project {
							uid
							project_uuid
							project_name
							project_is_admin: count(project_admins @filter(uid($userDgraphUID)))
						}
						task_assignee {
							user_uuid
							user_name
							user_profile_object_key
						}
						task_sub_task_count: count(task_sub_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z")))
						task_comment_count: count(task_comments @filter(not gt(comment_deleted_at, "1970-01-01T00:00:00Z")))
						`+dgraphStruct.TASK_BLOCKED_OPEN+`
						task_team {
							team_name
							team_uuid
						}
						task_created_at
					}
				}
			}`, filterQuery, filterQuery, sortQuery)

	dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserTaskList Failed to get user's task list from dgraph err: %+v",
			err,
		)
		return
	}
	return

}

func GetDgraphUserInfoByUUIDForSidebarNav(ctx context.Context, userUUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	variables := make(map[string]string)
	variables["$id"] = userUUID
	variables["$time"] = time.Now().Format(time.RFC3339Nano)

	query := `query UserInfo($id: string, $time: string){
				# Tasks of archived projects are not the user's work any more.
				var(func: eq(user_uuid, $id)) {
					user_projects @filter(gt(project_deleted_at, "1970-01-01T00:00:00Z")) { archivedProjects as uid }
				}
				userInfo(func: eq(user_uuid, $id)) {
					uid
					user_uuid
					user_name
					user_email_id
					user_status
					user_profile_object_key
				user_channels @filter( not gt(ch_deleted_at, "1970-01-01T00:00:00Z")){
					ch_uuid
					ch_name
					ch_posts (orderdesc: post_created_at, first: 1) {
						post_created_at
					}
				}
				user_fav_channels {
					ch_uuid
					ch_name
					ch_posts (orderdesc: post_created_at, first: 1) {
						post_created_at
					}
				}
					user_teams {
						team_name
						team_uuid
					}
					user_emoji_statuses @filter(gt(status_user_emoji_expiry_at, $time)) (orderdesc: status_user_emoji_expiry_at, first: 1) {
						uid
						status_user_emoji_id
						status_user_emoji_desc
						status_user_emoji_expiry_at
						status_user_emoji_expiry_in
					}
					user_dms {
						dm_grouping_id
						dm_participants {
							user_uuid
							user_name
							user_full_name
							is_bot
							user_profile_object_key
							user_device_connected
							user_status
							user_emoji_statuses @filter(gt(status_user_emoji_expiry_at, $time)) (orderdesc: status_user_emoji_expiry_at, first: 1) {
								uid
								status_user_emoji_id
								status_user_emoji_desc
								status_user_emoji_expiry_at
								status_user_emoji_expiry_in
							}
						}
						dm_chats (orderdesc:chat_created_at, first: 1) {
							chat_created_at
							
						}
					}
				user_task_count: count(user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects))))
				user_incomplete_task_count: count(user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) AND ` + dgraphStruct.TASK_OPEN_FILTER + ` AND not eq(task_status, "canceled")))
				user_overdue_task_count: count(user_tasks @filter(not gt(task_deleted_at, "1970-01-01T00:00:00Z") AND not uid_in(task_project, uid(archivedProjects)) AND ` + dgraphStruct.TASK_OPEN_FILTER + ` AND not eq(task_status, "canceled") AND lt(task_due_date, $time) AND gt(task_due_date, "1970-01-01T00:00:00Z")))
				user_docs: ~doc_created_by(orderdesc: doc_updated_at, first: 8) @filter(not gt(doc_deleted_at, "1970-01-01T00:00:00Z")) {
					doc_uuid
					doc_title
				}
				user_boards: ~board_created_by(orderdesc: board_updated_at, first: 8) @filter(not gt(board_deleted_at, "1970-01-01T00:00:00Z")) {
					board_uuid
					board_title
				}
				user_projects @filter(not gt(project_deleted_at, "1970-01-01T00:00:00Z")) {
					project_uuid
					project_name
				}
				user_theme_color
				user_theme_mode
			}
		}`

	dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUIDForSidebarNav Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphUserProjectList(ctx context.Context, userUUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	variables := make(map[string]string)
	variables["$id"] = userUUID

	query := `query UserInfo($id: string, $time: string){
				userInfo(func: eq(user_uuid, $id)) {
					uid
					user_uuid
					user_projects {
						project_uuid
						project_name
						project_deleted_at
						project_team {
							uid
							team_uuid
							team_name
						}
						uid
					}
				}
			}`

	dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserProjectList Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return

}

func GetDgraphUserInfoByUUID(ctx context.Context, userUUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {

	variables := make(map[string]string)
	variables["$id"] = userUUID
	variables["$time"] = time.Now().Format(time.RFC3339Nano)

	query := `query UserInfo($id: string, $time: string){
				userInfo(func: eq(user_uuid, $id)) {
					uid
					user_uuid
					user_name
					user_full_name
					user_email_id
					user_posts
					user_profile_object_key
					user_job_title
					user_hobbies
					user_device_connected
					user_status
					user_is_admin
					is_external
					is_bot
					user_channels {
						uid
						ch_uuid
					}
					user_emoji_statuses @filter(gt(status_user_emoji_expiry_at, $time)) (orderdesc: status_user_emoji_expiry_at, first: 1) {
						status_user_emoji_id
						status_user_emoji_desc
						status_user_emoji_expiry_at
						status_user_emoji_expiry_in
					}
					user_created_at
					user_updated_at
					user_deleted_at
					user_app_lang
				}
			}`

	dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUID Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	zero := 0
	if dgraphUser != nil && dgraphUser.Status == dgraphStruct.USER_OPT_STATUS_OFFLINE {
		dgraphUser.DevicesConnected = &zero
	}

	return
}

// GetDgraphUserInfoByDgraphUID resolves a user by their internal Dgraph
// node uid (e.g. "0x1a"), as opposed to the application UUID. This is the
// identity LiveKit participants carry (call tokens are minted with
// SetIdentity(dgraphUser.Uid)), so the meeting-recap agent uses it to map
// transcript speakers back to users.
func GetDgraphUserInfoByDgraphUID(ctx context.Context, dgraphUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	variables := make(map[string]string)
	variables["$uid"] = dgraphUID
	variables["$time"] = time.Now().Format(time.RFC3339Nano)

	query := `query UserInfo($uid: string, $time: string){
				userInfo(func: uid($uid)) @filter(type(User)) {
					uid
					user_uuid
					user_name
					user_full_name
					user_email_id
					user_profile_object_key
					user_is_admin
					is_external
					user_channels {
						uid
						ch_uuid
					}
					user_created_at
					user_deleted_at
				}
			}`

	dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query, variables)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByDgraphUID Failed to get user in dgraph err: %+v", err)
		return
	}
	return
}

func GetDgraphUserInfoByUUIDForMQTTConfig(ctx context.Context, userUUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {

	variables := make(map[string]string)
	variables["$id"] = userUUID
	query := `query UserInfo($id: string){
				userInfo(func: eq(user_uuid, $id)) {
					user_uuid
					user_channels {
						ch_uuid
					}
					user_dms {
						dm_grouping_id
					}
					user_projects {
						project_uuid
					}
				}
			}`

	dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUIDForMQTTConfig Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphUserInfoByUUIDs(ctx context.Context, userUUIDs []string) (dgraphUser []*dgraphStruct.DgraphUser, err error) {
	// Despite the name these are graph uids, written into the query below.
	if !dgraphquery.AllUIDs(userUUIDs) {
		return nil, errors.New("not a list of user ids")
	}

	variables := make(map[string]string)
	dgraphUids := strings.Join(userUUIDs, ", ")
	query := fmt.Sprintf(`query UserInfo(){
				userInfo(func: has(user_uuid)) @filter(uid(%+v)) {
					uid
					user_uuid
					user_name
					user_email_id
					user_posts
					user_profile_object_key
					user_job_title
					user_hobbies
					user_channels {
						ch_uuid
					}
					user_created_at
					user_updated_at
					user_deleted_at
				}
			}`, dgraphUids)

	dgraphUser, err = dgraphModels.GetDgraphUsersList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUIDs Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func DgraphUsersListNotExistInGivenChannel(ctx context.Context, channelUUID string) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {

	variables := make(map[string]string)
	variables["$ch_id"] = channelUUID
	query := `query UserInfo($ch_id: string){
				var(func: eq(ch_uuid, $ch_id)) {
				   c as uid
				}
				userInfo(func: has(user_uuid)) @filter(not uid_in(~ch_members, uid(c)) AND NOT eq(is_external, true)) {
					user_uuid
					user_name
					user_email_id
					user_posts
					user_profile_object_key
					user_job_title
					user_hobbies
					user_channels {
						ch_uuid
					}
					user_created_at
					user_updated_at
					user_deleted_at
				}
			}`

	dgraphUsers, err = dgraphModels.GetDgraphUsersList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUID Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetDgraphAllUsersList(ctx context.Context) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {

	variables := make(map[string]string)
	query := `query UserInfo(){

				userInfo(func: has(user_uuid)) @filter(NOT eq(is_external, true) OR eq(is_bot, true)) {
					uid
					user_uuid
					user_name
					user_full_name
					user_email_id
					user_posts
					user_profile_object_key
					user_job_title
					user_hobbies
					is_bot
					user_channels {
						ch_uuid
					}
					user_created_at
					user_updated_at
					user_deleted_at
				}
			}`

	dgraphUsers, err = dgraphModels.GetDgraphUsersList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DgraphAllUsersList Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

func GetAllUsersListFromDgraph(ctx context.Context, pageIndex int, pageSize int) (dgraphUsers []*dgraphStruct.DgraphUser, actualLen int, err error) {
	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize + 1)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$first"] = firstVal
	variables["$offset"] = offsetVal

	query := `query UserInfo($first: int, $offset: int){
				userInfo(func: has(user_uuid), first: $first, offset: $offset, orderdesc: user_created_at) @filter(NOT eq(is_external, true)) {
					user_uuid
					user_profile_object_key
					user_device_connected
					user_email_id
					user_status
					user_name
					user_full_name
					user_deleted_at
			  	}
			}`

	dgraphUsers, err = dgraphModels.GetDgraphUsersList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetAllUsersListFromDgraph Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	actualLen = len(dgraphUsers)
	return
}

func DeleteFavChannelEdge(ctx context.Context, userDgraphUID string, channelDgraphUID string) (err error) {

	delStringJSON := fmt.Sprintf(`{
		"uid": "%s",
		"user_fav_channels": {
			"uid": "%s"
		}
	}`, userDgraphUID, channelDgraphUID)

	err = dgraphModels.DeleteUserEdge(ctx, delStringJSON)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/DeleteFavChannelEdge Failed to remove fav channel in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

// GetDgraphUserInfoByEmailId finds the member with this address, for signing
// them in: never an external person or a bot.
//
// The address is matched in Postgres, without regard to case, and the graph
// node by its id. The graph can only match an address exactly, and accounts
// made before addresses were lowercased (helpers.NormalizeEmail) keep the
// case they arrived with, so asking it by address would sign nobody in whose
// provider spells their address differently from the way it was stored.
func GetDgraphUserInfoByEmailId(ctx context.Context, userEmailId string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var userID uuid.UUID
	err = postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		SELECT id FROM users
		WHERE LOWER(email_id) = LOWER($1) AND deleted_at IS NULL AND is_external = false AND is_bot = false
		`+sameAddressFirst, userEmailId).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphUserInfoByEmailId Failed to find the member err: %+v", err)
		return nil, err
	}

	variables := make(map[string]string)
	variables["$id"] = userID.String()
	query := `query UserInfo($id: string){
				userInfo(func: eq(user_uuid, $id)) @filter(not gt(user_deleted_at, "1970-01-01T00:00:00Z") AND NOT eq(is_external, true) AND NOT eq(is_bot, true)) {
					uid
					user_uuid
					user_name
					
				}
			}`

	dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByEmailId Failed to get user from dgraph err: %+v",
			err,
		)
		return
	}

	return
}
func GetUserRecordingsList(ctx context.Context, userDgraphUID string, startDate string, endDate string, pageIndex int, pageSize int) (recordings []*dgraphStruct.DgraphRecording, err error) {

	offset := pageIndex * pageSize
	firstVal := strconv.Itoa(pageSize + 1)
	offsetVal := strconv.Itoa(offset)

	variables := make(map[string]string)
	variables["$user_uid"] = userDgraphUID
	variables["$startDate"] = startDate
	variables["$endDate"] = endDate
	variables["$first"] = firstVal
	variables["$offset"] = offsetVal

	query := `query UserRecordings($user_uid: string, $startDate: string, $endDate: string, $first: int, $offset: int){
				var(func: uid($user_uid)) {
					~ch_members {
						ch_recs as ch_recording @filter(ge(recording_stared_at, $startDate) AND le(recording_stared_at, $endDate) AND not gt(recording_deleted_at, "1970-01-01T00:00:00Z") AND NOT eq(recording_transcript_only, true))
					}
					~dm_participants {
						dm_recs as dm_recording @filter(ge(recording_stared_at, $startDate) AND le(recording_stared_at, $endDate) AND not gt(recording_deleted_at, "1970-01-01T00:00:00Z") AND NOT eq(recording_transcript_only, true))
					}
				}
				recordingInfo(func: uid(ch_recs, dm_recs), orderdesc: recording_stared_at, first: $first, offset: $offset) {
					uid
					recording_egress_id
					recording_stared_at
					recording_ended_at
					recording_started_by {
						user_uuid
						user_name
						user_full_name
					}
					recording_duration
					recording_obj_key
					recording_size
					recording_channel {
						ch_uuid
						ch_name
					}
					recording_dm {
						dm_grouping_id
						dm_participants {
							user_uuid
							user_name
							user_full_name
							is_bot
						}
					}
				}
			}`

	recordings, err = recordingDgraphModels.GetDgraphRecordingsList(ctx, query, variables)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserRecordingsList Failed to get user recordings in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

// ===== Email Auth Domain Functions =====

// GetUserByEmailIdWithPassword loads a member's sign-in details by address.
// Members only: an external row or a bot's never signs in, even holding a
// password (accepting an invitation used to set one on an external row).
func GetUserByEmailIdWithPassword(ctx context.Context, emailID string) (userInfo *models.User, err error) {
	query := `
		SELECT id, email_id, password_hash, username, created_at, updated_at,
		       COALESCE(is_sso_managed, false), signup_method, last_login_method
		FROM users
		WHERE LOWER(email_id) = LOWER($1)
		AND deleted_at IS NULL
		AND is_external = false
		AND is_bot = false
		` + sameAddressFirst + `
	`
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var user models.User
	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, emailID).Scan(
		&user.Id,
		&user.EmailID,
		&user.PasswordHash,
		&user.Username,
		&user.CreatedAt,
		&user.UpdatedAt,
		&user.IsSSOManaged,
		&user.SignupMethod,
		&user.LastLoginMethod,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserByEmailIdWithPassword Failed to get user err: %+v", err)
		return
	}

	return &user, nil
}

func UpdatePasswordByUserID(ctx context.Context, userID uuid.UUID, passwordHash *string) (err error) {
	query := `
		UPDATE users
		SET password_hash = $1, password_generated = false, updated_at = NOW()
		WHERE id = $2
	`
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, passwordHash, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdatePasswordByUserID Failed to update password err: %+v", err)
		return
	}

	return
}

// MarkPasswordGenerated records that the account's password was generated for
// its owner rather than chosen by them. UpdatePasswordByUserID clears it.
func MarkPasswordGenerated(ctx context.Context, userID uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE users SET password_generated = true WHERE id = $1`, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MarkPasswordGenerated err: %+v", err)
	}
	return err
}

// PasswordGenerated reports whether the account still has a generated password.
func PasswordGenerated(ctx context.Context, userID uuid.UUID) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var generated bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT password_generated FROM users WHERE id = $1`, userID).Scan(&generated)
	return generated, err
}

// CreateUserWithMethod inserts a user record with full provenance.
//
// signupMethod is one of models.AuthMethod*. Empty string is treated as unknown
// (back-compat with callers pre-dating the migration).
//
// isSSOManaged=true means the user must authenticate via their external IdP
// going forward; SetPassword/ChangePassword will be rejected for them.
func CreateUserWithMethod(
	ctx context.Context,
	emailID string,
	username string,
	passwordHash *string,
	userUUID uuid.UUID,
	signupMethod string,
	isSSOManaged bool,
) (err error) {
	if err = EnsureSeatAvailable(ctx); err != nil {
		return err
	}
	query := `
		INSERT INTO users (id, email_id, username, password_hash, signup_method, is_sso_managed)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6)
	`
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, userUUID, emailID, username, passwordHash, signupMethod, isSSOManaged)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateUserWithMethod Failed to create user err: %+v", err)
		return
	}

	return
}

// AdoptExternalUser makes the external row an import or GitHub sync left for
// emailID the account of the person now joining with that address. It keeps
// its id, so the history imported under it (messages, tasks, comments) is
// theirs, and takes the sign-in method and password they joined with. Bots are
// external too, and never adopted.
//
// It reports false, changing nothing, when there's no such row. Adopting one
// adds a person, so it takes a seat as CreateUserWithMethod does, refusing a
// full workspace. The row is locked until both stores agree: Postgres commits
// only once the graph node is a member's too (member pickers and lists leave
// out anyone the graph marks external), and a sign-up racing this one waits,
// then finds nothing left to adopt.
func AdoptExternalUser(ctx context.Context, emailID string, passwordHash *string, signupMethod string, isSSOManaged bool) (userID uuid.UUID, adopted bool, err error) {
	c, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(c, nil)
	if err != nil {
		return uuid.Nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	err = tx.QueryRowContext(c, `
		SELECT id FROM users
		WHERE LOWER(email_id) = LOWER($1) AND is_external = true AND is_bot = false AND deleted_at IS NULL
		`+sameAddressFirst+`
		FOR UPDATE`, emailID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	if err = EnsureSeatAvailable(ctx); err != nil {
		return uuid.Nil, false, err
	}
	if _, err = tx.ExecContext(c, `
		UPDATE users
		SET is_external = false, password_hash = $2, signup_method = NULLIF($3, ''),
			is_sso_managed = $4, updated_at = NOW()
		WHERE id = $1`, userID, passwordHash, signupMethod, isSSOManaged); err != nil {
		return uuid.Nil, false, err
	}
	if err = setDgraphExternal(c, userID.String(), false); err != nil {
		return uuid.Nil, false, err
	}
	if err = tx.Commit(); err != nil {
		_ = helpers.CompensateOnFailure(ctx, "the member mark on the graph node of an adopted "+emailID,
			func(undoCtx context.Context) error { return setDgraphExternal(undoCtx, userID.String(), true) })
		return uuid.Nil, false, err
	}
	_ = redisStore.Delete(ctx, registry.UserProfile, []string{userID.String()})
	_ = redisStore.Delete(ctx, registry.UserDgraphProfile, []string{userID.String()})
	return userID, true, nil
}

// setDgraphExternal marks a person's graph node external, or makes it a
// member's: one without the mark.
func setDgraphExternal(ctx context.Context, userUUID string, external bool) error {
	mu := &api.Mutation{Cond: "@if(eq(len(u), 1))", DelNquads: []byte(`uid(u) <is_external> * .`)}
	if external {
		mu = &api.Mutation{Cond: "@if(eq(len(u), 1))", SetJson: []byte(`{"uid": "uid(u)", "is_external": true}`)}
	}
	_, err := dgraphInit.DoCommitNow(ctx, &api.Request{
		Query:     `query q($id: string) { found(func: eq(user_uuid, $id)) { u as uid } }`,
		Vars:      map[string]string{"$id": userUUID},
		Mutations: []*api.Mutation{mu},
	})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/setDgraphExternal err: %+v", err)
	}
	return err
}

// IsUniqueViolationOnUsername inspects an error from a postgres INSERT/UPDATE
// and reports whether it's specifically a unique-constraint violation on the
// users.username column. Used by the business layer to retry with a suffix
// instead of failing the user.
//
// Implemented via string match on the lib/pq error rather than typed
// inspection so we don't drag the lib/pq dependency into the business
// package; keeps the layering clean.
func IsUniqueViolationOnUsername(err error) bool {
	return uniqueViolationTargets(err, "username")
}

// IsUniqueViolationOnEmail reports whether err is Postgres refusing a duplicate email_id.
//
// users carries TWO unique columns — email_id and username — and they mean opposite things to a
// caller. A username collision is ours to resolve, which is why CreateUserWithMethod retries with
// a suffix. An email collision is not: that address already belongs to an account, and the only
// correct responses are to tell the user or, where the email is proven, to adopt the existing row.
// Conflating them would either retry something unretryable or refuse something recoverable.
func IsUniqueViolationOnEmail(err error) bool {
	return uniqueViolationTargets(err, "email_id")
}

// uniqueViolationTargets reports whether err is a unique violation concerning column.
//
// This reads the DRIVER's structured error rather than matching the message text. The previous
// implementation did:
//
//	msg := strings.ToLower(err.Error())
//	return strings.Contains(msg, "duplicate key value") && strings.Contains(msg, "username")
//
// which worked only by accident of the constraint being named "users_username_key". It would stop
// matching if the constraint were renamed, if Postgres reworded the message, or if it ran under a
// non-English locale — and the failure is silent and awkward: the username-collision retry loop in
// CreateUserWithMethod simply stops retrying, so SSO provisioning starts failing for exactly the
// directory collisions that loop exists to absorb. It would also match too eagerly, since any
// message merely CONTAINING "username" qualified.
//
// A *pq.Error carries the code, the constraint and the column as fields, so the check can be
// exact. errors.As is used rather than a type assertion because these errors travel up through
// several wrapping layers.
func uniqueViolationTargets(err error, column string) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return false
	}
	if string(pqErr.Code) != "23505" { // unique_violation
		return false
	}
	// Column is populated for some violations and empty for others; the constraint name is the
	// reliable signal for a unique index, and Postgres names it "<table>_<column>_key" by default.
	if pqErr.Column == column {
		return true
	}
	return strings.Contains(strings.ToLower(pqErr.Constraint), strings.ToLower(column))
}

// RecordLoginMethod updates last_login_method and last_login_at for audit/UX.
// Best-effort: callers should log but not fail the login on error.
func RecordLoginMethod(ctx context.Context, userID uuid.UUID, method string) error {
	query := `
		UPDATE users
		SET last_login_method = $1, last_login_at = NOW(), updated_at = NOW()
		WHERE id = $2
	`
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, method, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/RecordLoginMethod Failed to record login method err: %+v", err)
	}
	return err
}

// GetUserAuthFlagsByUUID loads only the auth-related flags + email for a user
// by UUID. Cheaper than the full GetUserByUUID and includes is_sso_managed
// which the legacy loader doesn't.
func GetUserAuthFlagsByUUID(ctx context.Context, userID uuid.UUID) (*models.User, error) {
	query := `
		SELECT id, email_id, COALESCE(is_sso_managed, false), signup_method, last_login_method
		FROM users
		WHERE id = $1
		AND deleted_at IS NULL
	`
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var user models.User
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, userID).Scan(
		&user.Id,
		&user.EmailID,
		&user.IsSSOManaged,
		&user.SignupMethod,
		&user.LastLoginMethod,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetUserAuthFlagsByUUID err: %+v", err)
		return nil, err
	}
	return &user, nil
}

func GetInvitationByToken(ctx context.Context, token string) (invitation *models.Invitation, err error) {
	query := `
		SELECT id, email, invited_by, status, token, token_expires_at, created_at
		FROM invitations
		WHERE token = $1
	`
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var inv models.Invitation
	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, token).Scan(
		&inv.Id,
		&inv.Email,
		&inv.InvitedBy,
		&inv.Status,
		&inv.Token,
		&inv.TokenExpiresAt,
		&inv.CreatedAt,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetInvitationByToken Failed to get invitation err: %+v", err)
		return
	}

	return &inv, nil
}

func UpdateInvitationStatus(ctx context.Context, invitationID uuid.UUID, status string) (err error) {
	query := `
		UPDATE invitations
		SET status = $1
		WHERE id = $2
	`
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, status, invitationID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateInvitationStatus Failed to update invitation status err: %+v", err)
		return
	}

	return
}

func AddInvitationWithToken(ctx context.Context, email string, invitedBy uuid.UUID, token string, expiresAt time.Time) (err error) {
	query := `
		INSERT INTO invitations (email, invited_by, status, token, token_expires_at)
		VALUES ($1, $2, 'sent', $3, $4)
	`
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, email, invitedBy, token, expiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/AddInvitationWithToken Failed to add invitation err: %+v", err)
		return
	}

	return
}

func CheckIfAnyAdminExists(ctx context.Context) (exists bool, err error) {
	query := `
		SELECT EXISTS (
			SELECT 1 FROM admin_users LIMIT 1
		)
	`
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query).Scan(&exists)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CheckIfAnyAdminExists Failed to check admin existence err: %+v", err)
		return
	}

	return
}

// UpdateInvitationTokenByID gives one invitation a new link and expiry, from
// invitedBy, who renewed it: the invitation is theirs now, and its email
// names them.
//
// By id, not by address: an address can have more than one invitation row
// (made before inviting a member twice was refused), and setting every one of
// them to the same new token broke on token's unique index, so sending such
// an invitation again failed. The caller renews the newest
// (GetInvitationByEmail).
func UpdateInvitationTokenByID(ctx context.Context, id uuid.UUID, token string, expiresAt time.Time, invitedBy uuid.UUID) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		UPDATE invitations
		SET token = $1, token_expires_at = $2, status = 'sent', invited_by = $4
		WHERE id = $3`, token, expiresAt, id, invitedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateInvitationTokenByID Failed to update invitation token err: %+v", err)
	}
	return err
}

// GetInvitationByEmail is the newest invitation for an address, the one that
// is renewed and judged, or nil when there is none.
func GetInvitationByEmail(ctx context.Context, email string) (*models.Invitation, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var inv models.Invitation
	err := postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		SELECT id, email, invited_by, status, token, token_expires_at, created_at
		FROM invitations
		WHERE LOWER(email) = LOWER($1)
		ORDER BY created_at DESC
		LIMIT 1`, email).Scan(&inv.Id, &inv.Email, &inv.InvitedBy, &inv.Status, &inv.Token, &inv.TokenExpiresAt, &inv.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetInvitationByEmail err: %+v", err)
		return nil, err
	}
	return &inv, nil
}

// HasUsableInvitation reports whether email holds an invitation that can
// still let someone in, as models.Invitation.LiveAt decides: the rows are
// read and judged by that one predicate, not by a copy of it in SQL.
func HasUsableInvitation(ctx context.Context, email string) (bool, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT id, email, invited_by, status, token, token_expires_at, created_at
		FROM invitations
		WHERE LOWER(email) = LOWER($1)`, email)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/HasUsableInvitation err: %+v", err)
		return false, err
	}
	defer rows.Close()
	now := time.Now()
	usable := false
	for rows.Next() {
		var inv models.Invitation
		if err := rows.Scan(&inv.Id, &inv.Email, &inv.InvitedBy, &inv.Status, &inv.Token, &inv.TokenExpiresAt, &inv.CreatedAt); err != nil {
			helpers.LogErrorWithContext(ctx, "domain/HasUsableInvitation scan err: %+v", err)
			return false, err
		}
		usable = usable || inv.LiveAt(now)
	}
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/HasUsableInvitation rows err: %+v", err)
		return false, err
	}
	return usable, nil
}

// MarkInvitationJoined records that the person invited at email has joined,
// however they came in. Nothing to mark is not an error.
func MarkInvitationJoined(ctx context.Context, email string) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		UPDATE invitations SET status = 'joined'
		WHERE LOWER(email) = LOWER($1) AND status <> 'joined'`, email)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MarkInvitationJoined err: %+v", err)
	}
	return err
}

// MemberAccountState reports whether a member's account has this address,
// and whether it is deactivated. External rows and bots are not members.
func MemberAccountState(ctx context.Context, email string) (exists bool, deactivated bool, err error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	err = postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		SELECT deleted_at IS NOT NULL FROM users
		WHERE LOWER(email_id) = LOWER($1) AND is_external = false AND is_bot = false
		ORDER BY (deleted_at IS NULL) DESC, created_at ASC
		LIMIT 1`, email).Scan(&deactivated)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MemberAccountState err: %+v", err)
		return false, false, err
	}
	return true, deactivated, nil
}

// GetUserIDByEmail finds a user ID by email address.
func GetUserIDByEmail(ctx context.Context, email string) (uuid.UUID, error) {
	query := `SELECT id FROM users WHERE LOWER(email_id) = LOWER($1) AND deleted_at IS NULL ` + sameAddressFirst
	userID, err := models.GetUserIDByEmail(query, email)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetUserIDByEmail Failed err: %+v", err)
		return uuid.Nil, err
	}
	return userID, nil
}

// CreateExternalGitHubUser creates a stub user for an unmapped GitHub user.
func CreateExternalGitHubUser(ctx context.Context, userUUID uuid.UUID, emailID string, login string, displayName *string, avatarURL *string, htmlURL *string) error {
	query := `
		INSERT INTO users (id, email_id, is_external, github_login, display_name, github_avatar_url, github_html_url, created_at, updated_at)
		VALUES ($1, $2, true, $3, $4, $5, $6, NOW(), NOW())
	`
	err := models.CreateExternalGitHubUser(query, userUUID, emailID, login, displayName, avatarURL, htmlURL)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CreateExternalGitHubUser Failed err: %+v", err)
		return err
	}
	return nil
}

// GetUserByGitHubLogin finds a user by their GitHub login (case-insensitive).
func GetUserByGitHubLogin(ctx context.Context, login string) (*models.User, error) {
	query := `
		SELECT id, email_id, created_at, updated_at, deleted_at, is_admin, is_external, github_login, github_avatar_url, github_html_url, display_name
		FROM users
		WHERE LOWER(github_login) = LOWER($1) AND deleted_at IS NULL
		LIMIT 1
	`
	user, err := models.GetUserByGitHubLogin(query, login)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetUserByGitHubLogin Failed err: %+v", err)
		return nil, err
	}
	return user, nil
}

// GetExternalUsers returns a paginated list of external (ghost) users.
func GetExternalUsers(ctx context.Context, pageIndex int, pageSize int) (usersInfo []*models.User, actualLen int, err error) {
	offset := pageIndex * pageSize
	query := `
		SELECT id, email_id, created_at, updated_at, deleted_at, is_admin, is_external, github_login, github_avatar_url, github_html_url, display_name
		FROM users
		WHERE is_external = true AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2
	`

	usersInfo, err = models.GetExternalUsers(query, pageSize+1, offset)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetExternalUsers Failed to get external users err: %+v",
			err)
		return
	}

	actualLen = len(usersInfo)
	return
}

// UnlinkExternalUser clears GitHub fields for an external user.
// Returns true if a row was actually updated.
func UnlinkExternalUser(ctx context.Context, userUUID uuid.UUID) (bool, error) {
	rowsAffected, err := models.UnlinkExternalUser(userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UnlinkExternalUser Failed err: %+v",
			err)
		return false, err
	}
	return rowsAffected > 0, nil
}

// GetActiveUsersByUUIDsForNotification batches the (id, email,
// is_external) projection used by the notification dispatcher fan-out.
//
// One DB roundtrip regardless of recipient count. Missing keys signal
// "user not found / deleted" and the dispatcher should skip them.
func GetActiveUsersByUUIDsForNotification(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*models.User, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]*models.User{}, nil
	}
	out, err := models.GetActiveUsersByUUIDsForNotification(ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetActiveUsersByUUIDsForNotification err: %+v", err)
		return nil, err
	}
	return out, nil
}

// GetActiveDgraphUsersByUUIDsLight is a batched, minimal-fields
// counterpart to GetActiveDgraphUserInfoByUUID. Accepts a slice of
// user UUIDs and returns one Dgraph user per resolved UUID with just
// the fields needed for participant edges (uid, user_uuid, user_name,
// user_profile_object_key).
//
// Use this when you have N participants and would otherwise issue N
// single GetActiveDgraphUserInfoByUUID calls in a loop — that's the
// hot path on calendar-event create / update for ~50-person events.
//
// Returns an empty slice (no error) for an empty input. Soft-deleted
// users are filtered out. Order is NOT guaranteed; callers that need
// a per-uuid map should build one from the result.
func GetActiveDgraphUsersByUUIDsLight(ctx context.Context, userUUIDs []string) ([]*dgraphStruct.DgraphUser, error) {
	// Dedup + drop empties so the IN-list stays minimal.
	seen := make(map[string]struct{}, len(userUUIDs))
	clean := make([]string, 0, len(userUUIDs))
	for _, u := range userUUIDs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		seen[u] = struct{}{}
		clean = append(clean, u)
	}
	if len(clean) == 0 {
		return nil, nil
	}

	// DQL doesn't support inline list parameters with parameterised
	// queries for `eq(field, [a, b, c])`, so we build the list as a
	// quoted string. Each value is a UUID — already validated upstream —
	// but we apply a defensive char-set filter so a stray non-UUID
	// caller can't inject DQL.
	parts := make([]string, 0, len(clean))
	for _, u := range clean {
		safe := true
		for _, r := range u {
			ok := (r >= 'a' && r <= 'z') ||
				(r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') ||
				r == '-'
			if !ok {
				safe = false
				break
			}
		}
		if safe {
			parts = append(parts, `"`+u+`"`)
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}

	query := fmt.Sprintf(`{
		userInfo(func: eq(user_uuid, [%s])) @filter(not gt(user_deleted_at, "1970-01-01T00:00:00Z")) {
			uid
			user_uuid
			user_name
			user_full_name
			user_email_id
			user_profile_object_key
		}
	}`, strings.Join(parts, ", "))

	users, err := dgraphModels.GetDgraphUsersList(ctx, query, map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetActiveDgraphUsersByUUIDsLight failed err: %+v", err)
		return nil, err
	}
	return users, nil
}

// GetUserDisplayMapByUUIDs resolves a set of user UUIDs to display info in a
// SINGLE batch query and returns a uuid -> user map, so callers that need to
// decorate a list (snapshot contributors, viewers, mentions, ...) can do it
// without an N+1 per-user lookup loop. Soft-deleted and unresolved users are
// simply absent from the map. Safe for an empty/nil input (returns an empty
// map).
func GetUserDisplayMapByUUIDs(ctx context.Context, userUUIDs []string) (map[string]*dgraphStruct.DgraphUser, error) {
	out := make(map[string]*dgraphStruct.DgraphUser)
	if len(userUUIDs) == 0 {
		return out, nil
	}
	users, err := GetActiveDgraphUsersByUUIDsLight(ctx, userUUIDs)
	if err != nil {
		return out, err
	}
	for _, u := range users {
		if u != nil && u.Uuid != "" {
			out[u.Uuid] = u
		}
	}
	return out, nil
}

// UserDisplay is the minimal, generic identity shape used to decorate lists
// (snapshot contributors, viewers, mentions, ...). Its JSON tags match the
// fields the frontend already consumes, so it can be embedded or used directly
// in API responses.
type UserDisplay struct {
	Uuid       string `json:"user_uuid"`
	FullName   string `json:"user_full_name"`
	Name       string `json:"user_name"`
	ProfileKey string `json:"user_profile_object_key"`
	// Display is the name people see, by the one rule
	// (helpers.PersonDisplayName).
	Display string `json:"-"`
}

// ResolveUserDisplays resolves a set of user UUIDs to display info in a SINGLE
// batch query and returns a uuid -> UserDisplay map. This is the generic,
// allocation-light replacement for any N+1 "resolve each user in a loop"
// pattern. Unresolved/soft-deleted users are absent from the map.
func ResolveUserDisplays(ctx context.Context, userUUIDs []string) (map[string]UserDisplay, error) {
	userMap, err := GetUserDisplayMapByUUIDs(ctx, userUUIDs)
	if err != nil {
		return map[string]UserDisplay{}, err
	}
	out := make(map[string]UserDisplay, len(userMap))
	for uuidStr, u := range userMap {
		profileKey := ""
		if u.ProfileKey != nil {
			profileKey = *u.ProfileKey
		}
		out[uuidStr] = UserDisplay{
			Uuid:       u.Uuid,
			FullName:   u.UserFullName,
			Name:       u.UserName,
			ProfileKey: profileKey,
			Display:    u.DisplayName(),
		}
	}
	return out, nil
}

// HardDeleteUser removes a user row outright, and clears its cache entries.
//
// Compensation only: see models.HardDeleteUser for why a soft delete cannot serve (email_id and
// username are both UNIQUE and a soft-deleted row keeps them), and for why this is safe only in
// the window between the insert and the Dgraph write.
// No cache invalidation here, deliberately. The channel equivalent clears a Redis entry because
// UpdateChannelInfo populates one, but nothing caches a user at creation time — registry.
// UserProfile is declared and currently read and written by nothing — and a row inserted
// microseconds earlier, whose Dgraph write then failed, has never been read into any cache.
// Inventing an invalidation here would be code defending against a state that cannot exist.
func HardDeleteUser(ctx context.Context, userUUID uuid.UUID) (err error) {
	query := `DELETE FROM users WHERE id = $1`
	err = models.HardDeleteUser(query, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/HardDeleteUser Failed to delete user row err: %+v", err)
		return
	}
	return
}

// SearchPeople finds workspace members by name, full name, job title or
// department, for the AI people-lookup tool.
//
// SEPARATE FROM GetUserListWithSearchText, which searches user_name alone and is
// wired into the mention typeahead and the share pickers. Widening that query
// would change what those surfaces return, and a picker that suddenly matches on
// department is a surprise nobody asked for. This one is additive.
//
// BOTS ARE EXCLUDED. Every Agent Builder agent has a real users row so it can
// author as itself, so without this filter "who works on billing" answers with
// the agents. Confirmed against a live workspace: two of them show up.
//
// The empty-field case is deliberate rather than unhandled. job title and
// department are unset on every user in the workspaces I can see, so those two
// clauses match nothing today; they cost one filter each and start working the
// day somebody fills the fields in.
//
// The needle is escaped by the same helper the other search uses, because this
// one is called with text a model produced.
func SearchPeople(ctx context.Context, needle string, limit int) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {
	safe := escapeForDgraphRegex(needle)
	if safe == "" {
		return nil, nil
	}
	if limit <= 0 || limit > peopleSearchMaxResults {
		limit = peopleSearchMaxResults
	}

	// THE BLOCK MUST BE CALLED userInfo. GetDgraphUsersList unmarshals into a
	// struct with one field tagged `json:"userInfo"`, so any other name parses
	// cleanly into an empty slice: no error, no rows, a tool that always answers
	// "nobody found". Named it "people" first and caught it by reading the model.
	query := fmt.Sprintf(`{
			userInfo(func: has(user_uuid), first: %d) @filter(
				(regexp(user_name, /.*%s.*/i)
				 OR regexp(user_full_name, /.*%s.*/i)
				 OR regexp(user_job_title, /.*%s.*/i)
				 OR regexp(<user.department>, /.*%s.*/i))
				AND NOT eq(is_external, true)
				AND NOT eq(is_bot, true)) {
				user_uuid
				user_name
				user_full_name
				user_job_title
				user.department
				user_email_id
			}
			}`, limit, safe, safe, safe, safe)

	dgraphUsers, err = dgraphModels.GetDgraphUsersList(ctx, query, map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SearchPeople failed err: %+v", err)
		return nil, err
	}
	return dgraphUsers, nil
}

// peopleSearchMaxResults bounds what a single lookup returns. A directory is not
// something an agent should be able to page through one tool call at a time.
const peopleSearchMaxResults = 15
