package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v230"
	"github.com/dgraph-io/dgo/v230/protos/api"
	"github.com/lib/pq"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/models/dgexec"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	recordingDgraphModels "github.com/akashc777/OneCamp/models/dgraph/Recording"
	dgraphTaskModels "github.com/akashc777/OneCamp/models/dgraph/Task"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/User"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	OpenSearchModels "github.com/akashc777/OneCamp/models/openSearch/User"
	"github.com/akashc777/OneCamp/models/pgexec"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/akashc777/OneCamp/models/taskread"
	"github.com/google/uuid"
)

const BEFORE_THIRTY_DAYS = -30 * 24 * time.Hour

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

// graphUID is a Dgraph node uid as Dgraph writes one.
var graphUID = regexp.MustCompile(`^0x[0-9a-f]+$`)

// UserNamesByDgraphUIDs is each person's display name by their graph uid,
// for a call's speakers (LiveKit identities are graph uids): one narrow read
// for all of them, where each was a read of the whole person (backend audit
// #22). Anything that isn't a uid is left out; so is anything that isn't a
// person.
func UserNamesByDgraphUIDs(ctx context.Context, uids []string) (map[string]string, error) {
	// A transcript names its speaker on every line: each uid goes into the
	// read once, however many lines carry it.
	keep := make([]string, 0, len(uids))
	seen := make(map[string]bool, len(uids))
	for _, u := range uids {
		if graphUID.MatchString(u) && !seen[u] {
			seen[u] = true
			keep = append(keep, u)
		}
	}
	out := make(map[string]string, len(keep))
	if len(keep) == 0 {
		return out, nil
	}
	people, err := dgexec.List[struct {
		UID  string `json:"uid"`
		Name string `json:"user_name"`
	}](ctx, `{ people(func: uid(`+strings.Join(keep, ", ")+`)) @filter(has(user_uuid)) { uid user_name } }`, nil, "people")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UserNamesByDgraphUIDs err: %+v", err)
		return nil, err
	}
	for _, p := range people {
		out[p.UID] = p.Name
	}
	return out, nil
}

// MembersByAddress is the member (live, not external) each of these
// addresses belongs to, by address in lower case, chosen as GetUserByEmailId
// chooses for a lower-case address: the all-lowercase account, else the
// oldest. One statement for an import's people, where each was one.
func MembersByAddress(ctx context.Context, emails []string) (map[string]uuid.UUID, error) {
	if len(emails) == 0 {
		return map[string]uuid.UUID{}, nil
	}
	lower := make([]string, len(emails))
	for i, e := range emails {
		lower[i] = strings.ToLower(e)
	}
	out, err := pgexec.Pairs[string, uuid.UUID](ctx, `
		SELECT DISTINCT ON (LOWER(email_id)) LOWER(email_id), id
		FROM users
		WHERE LOWER(email_id) = ANY($1) AND deleted_at IS NULL AND is_external = false
		ORDER BY LOWER(email_id), (email_id = LOWER(email_id)) DESC, created_at ASC`, pq.Array(lower))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MembersByAddress err: %+v", err)
	}
	return out, err
}

// GetUserSignInFlags reads whether a person is an admin and whether single
// sign-on manages their account: what a directory sign-in needs to keep the
// admin role in step with the directory's groups.
func GetUserSignInFlags(ctx context.Context, userID uuid.UUID) (isAdmin, isSSOManaged bool, err error) {
	type flags struct{ admin, sso bool }
	f, err := one(ctx, `
		SELECT (au.id IS NOT NULL), COALESCE(u.is_sso_managed, false)
		FROM users u
		LEFT JOIN admin_users au ON au.email_id = u.email_id
		WHERE u.id = $1
		LIMIT 1`, func(r pgexec.Row) (f flags, err error) { err = r.Scan(&f.admin, &f.sso); return }, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetUserSignInFlags err: %+v", err)
	}
	return f.admin, f.sso, err
}

// FirstSignInOfProvisioned reports whether this person's account was made
// for them by the directory (SCIM) and they have never signed in: their
// first sign-in is when they arrive, as joining is for everyone else. A read
// failure reads as no.
func FirstSignInOfProvisioned(ctx context.Context, userID uuid.UUID) bool {
	first, err := pgexec.Scalar[bool](ctx, `
		SELECT COALESCE(signup_method, '') = $2 AND last_login_at IS NULL
		FROM users WHERE id = $1`, userID, models.AuthMethodSCIM)
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
	var handleVar []string
	handleBlock := ""
	if len(byHandle) > 0 {
		list := dgraphquery.UUIDListOf(byHandle)
		match += ` OR eq(user_uuid, [` + list + `])`
		handleBlock = `byHandle as var(func: eq(user_uuid, [` + list + `]))`
		handleVar = []string{"byHandle"}
	}
	// From the people whose search name matches (and the handles' people)
	// when the term is long enough for its index, not from every person; the
	// filter still decides who matches (searchName.go).
	vars, root := dgraphquery.RegexpUnionRoot(safeSearch, "has(user_uuid)", "named", searchNamePredicates, handleVar...)
	if root == "has(user_uuid)" {
		handleBlock = ""
	}
	query := fmt.Sprintf(`query UserInfo($user_id: string){
			%s%s
			userInfo(func: %s) @filter(has(user_uuid) AND (%s) AND NOT eq(is_external, true)) {
				uid
				user_uuid
				user_name
				user_full_name
				user_profile_object_key
				user_device_connected
				user_email_id
				user_status
		  	}
			}`, vars, handleBlock, root, match)

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
	escaped := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.ToLower(term))
	ids, err := pgexec.Query(ctx, `
		SELECT id FROM users
		WHERE LOWER(username) LIKE $1 ESCAPE '\'
		  AND is_external = false AND is_bot = false AND deleted_at IS NULL
		ORDER BY username LIMIT $2`, scanUUID, "%"+escaped+"%", limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MemberIDsWithHandleLike err: %+v", err)
		return nil, err
	}
	return ids, nil
}

// HandlesByID is the handle of each of these people who has one, by id.
func HandlesByID(ctx context.Context, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	out, err := pgexec.Pairs[string, string](ctx, `
		SELECT id::text, username FROM users
		WHERE id = ANY($1::uuid[]) AND username IS NOT NULL AND username <> ''`, pq.Array(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/HandlesByID err: %+v", err)
		return nil, err
	}
	return out, nil
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

// profileSettle is how long after a change forgetProfile forgets a person's
// cached profile a second time. A request that read the database just before
// the change, and wrote what it read to the cache just after the first forget,
// would otherwise serve the old row for the cache's whole hour: an admin's flag
// after a demotion, an active account after a deactivation. That read takes
// milliseconds; this is far longer.
var profileSettle = 2 * time.Second

// forgetProfile drops what is cached about each person: their account row,
// which carries whether they are an admin and whether the account is active
// (GetActiveUserWithAdminFlagByUserUUID, read on every authenticated request),
// and their graph profile. It is the one way a change to either reaches the
// next request at once, on whichever server takes it (the cache is shared), so
// every write that changes who someone is or what they may do ends here. It
// forgets once more after profileSettle, for a read that was in flight. That
// second forget runs on the timer's own goroutine, where a panic would end the
// server, so it goes through GoSafeNamed, which recovers and logs one.
func forgetProfile(ctx context.Context, userUUIDs ...string) {
	var ids []string
	for _, id := range userUUIDs {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	forget := func(ctx context.Context) {
		for _, id := range ids {
			_ = redisStore.Delete(ctx, registry.UserProfile, []string{id})
			_ = redisStore.Delete(ctx, registry.UserDgraphProfile, []string{id})
		}
	}
	forget(ctx)
	later := context.WithoutCancel(ctx)
	time.AfterFunc(profileSettle, func() {
		helpers.GoSafeNamed("forgetProfile", func() { forget(later) })
	})
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

	adminRoleChanged(ctx, emailId, userUUID...)
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

	adminRoleChanged(ctx, emailId, userUUID...)
	return
}

// adminRoleChanged forgets the cached profile of every account a change to
// emailID's admin role reaches. admin_users is keyed by address, so that is
// whoever holds the address, and the account the caller already has in hand
// (knownUUID) besides.
func adminRoleChanged(ctx context.Context, emailID string, knownUUID ...string) {
	ids := append([]string(nil), knownUUID...)
	if user, err := GetUserByEmailId(ctx, &emailID); err == nil && user != nil {
		ids = append(ids, user.Id.String())
	}
	forgetProfile(ctx, ids...)
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
	out, err := pgexec.Query(ctx, `
		SELECT LOWER(email_id) FROM users
		WHERE is_external = false AND is_bot = false
		GROUP BY LOWER(email_id) HAVING count(*) > 1
		ORDER BY 1 LIMIT $1`, scanString, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/AddressesWithMoreThanOneAccount err: %+v", err)
		return nil, err
	}
	return out, nil
}

// TakenHandles is every handle in use that a new one based on base could
// collide with: base itself and base-<anything>, compared without case.
func TakenHandles(ctx context.Context, base string) ([]string, error) {
	escaped := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.ToLower(base))
	taken, err := pgexec.Query(ctx, `
		SELECT LOWER(username) FROM users
		WHERE LOWER(username) = LOWER($1) OR LOWER(username) LIKE $2 ESCAPE '\'`, scanString, base, escaped+"-%")
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/TakenHandles err: %+v", err)
		return nil, err
	}
	return taken, nil
}

// GetActiveUserIDByHandle is the id of the active member whose handle
// (users.username) is handle, compared without case and without a leading
// "@", or uuid.Nil when nobody has it.
func GetActiveUserIDByHandle(ctx context.Context, handle string) (uuid.UUID, error) {
	handle = strings.TrimPrefix(strings.TrimSpace(handle), "@")
	if handle == "" {
		return uuid.Nil, nil
	}
	id, _, err := pgexec.QueryOne(ctx, `
		SELECT id FROM users
		WHERE LOWER(username) = LOWER($1) AND deleted_at IS NULL AND is_external = false
		LIMIT 1`, scanUUID, handle)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetActiveUserIDByHandle err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// GetHandle is a person's handle (users.username), or "" when they have none.
func GetHandle(ctx context.Context, userID uuid.UUID) (string, error) {
	handle, err := pgexec.Scalar[sql.NullString](ctx, `SELECT username FROM users WHERE id = $1`, userID)
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
	out, err := pgexec.Query(ctx, `
		SELECT id, email_id, COALESCE(created_at, 'epoch') AS joined FROM users
		WHERE (username IS NULL OR username = '')
		  AND is_external = false AND is_bot = false AND deleted_at IS NULL
		  AND (COALESCE(created_at, 'epoch'), id) > ($1, $2)
		ORDER BY joined, id LIMIT $3`, func(r pgexec.Row) (m MemberWithoutHandle, err error) {
		err = r.Scan(&m.ID, &m.Email, &m.JoinedAt)
		return
	}, after.JoinedAt, after.ID, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MembersWithoutHandle err: %+v", err)
		return nil, err
	}
	return out, nil
}

// HandleTakenByAnother reports whether someone other than userID has this
// handle, compared without case.
func HandleTakenByAnother(ctx context.Context, handle string, userID uuid.UUID) (bool, error) {
	taken, err := pgexec.Scalar[bool](ctx, `
		SELECT EXISTS (SELECT 1 FROM users WHERE LOWER(username) = LOWER($1) AND id <> $2)`, handle, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/HandleTakenByAnother err: %+v", err)
	}
	return taken, err
}

// SetHandle changes someone's handle. A unique violation means another person
// has it.
func SetHandle(ctx context.Context, userID uuid.UUID, handle string) error {
	_, err := pgexec.Exec(ctx, `UPDATE users SET username = $2, updated_at = NOW() WHERE id = $1`, userID, handle)
	return err
}

// SetHandleIfMissing gives someone without a handle this one, and reports
// whether it did. A unique violation means another person took it first.
func SetHandleIfMissing(ctx context.Context, userID uuid.UUID, handle string) (bool, error) {
	n, err := pgexec.Exec(ctx, `
		UPDATE users SET username = $2, updated_at = NOW()
		WHERE id = $1 AND (username IS NULL OR username = '')`, userID, handle)
	return n > 0, err
}

// ExternalName is the name an import or a GitHub sync gave the person at
// this address, on the external row it left for them, or "" for none.
func ExternalName(ctx context.Context, email string) (string, error) {
	name, _, err := pgexec.QueryOne(ctx, `
		SELECT COALESCE(NULLIF(display_name, ''), username) FROM users
		WHERE LOWER(email_id) = LOWER($1) AND is_external = true AND is_bot = false AND deleted_at IS NULL
		`+sameAddressFirst, func(r pgexec.Row) (n sql.NullString, err error) { err = r.Scan(&n); return }, email)
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

// IndexUserInOpenSearch writes a new person's whole search document, so they
// can be found from the moment they exist, even in an index a reset emptied.
func IndexUserInOpenSearch(openSearchUser *openSearchStruct.OpenSearchUser) {
	ctx := context.Background()
	if err := OpenSearchModels.IndexUserInOpenSearch(ctx, openSearchUser); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/IndexUserInOpenSearch Failed to index user in opensearch err: %+v", err)
	}
}

func GetAllInvitations(ctx context.Context) (invitations []*models.Invitation, err error) {
	invitations, err = pgexec.Query(ctx, `
		SELECT `+invitationColumns+`
		FROM invitations
		ORDER BY created_at DESC`, scanInvitation)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetAllInvitations Failed to get invitations err: %+v", err)
	}
	return
}

// scanInvitation reads an invitation's invitationColumns.
func scanInvitation(r pgexec.Row) (*models.Invitation, error) {
	var inv models.Invitation
	err := r.Scan(&inv.Id, &inv.Email, &inv.InvitedBy, &inv.Status, &inv.Link.Hash, &inv.Link.Sealed, &inv.TokenExpiresAt, &inv.CreatedAt)
	return &inv, err
}

// scanUUID and scanString read one row's id or text, for pgexec.
func scanUUID(r pgexec.Row) (id uuid.UUID, err error) { err = r.Scan(&id); return }

func scanString(r pgexec.Row) (v string, err error) { err = r.Scan(&v); return }

// invitationColumns are what scanInvitation reads, in its order.
const invitationColumns = `id, email, invited_by, status, token_hash, token_sealed, token_expires_at, created_at`

// one is a single-row read that answers sql.ErrNoRows for no row, as the
// callers of these functions test for.
func one[T any](ctx context.Context, q string, scan func(pgexec.Row) (T, error), args ...any) (T, error) {
	v, found, err := pgexec.QueryOne(ctx, q, scan, args...)
	if err == nil && !found {
		err = sql.ErrNoRows
	}
	return v, err
}

func DeleteInvitationByEmail(ctx context.Context, email string) (err error) {
	if _, err = pgexec.Exec(ctx, `DELETE FROM invitations WHERE LOWER(email) = LOWER($1)`, email); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/DeleteInvitationByEmail Failed to delete invitation err: %+v", err)
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

// EmojiStatusesKept is how many statuses a person keeps: the one in force and
// the last few before it, which the status menu offers again. Setting one
// deletes the oldest beyond it.
const EmojiStatusesKept = 5

// emojiStatusOrder puts the status set last first. One set before set times
// were kept (16 Oct 2026) has none and comes after every one that has. One
// key only: Dgraph refuses a second key where the first ties beside a status
// without one ("Arguments of different type can not be compared").
const emojiStatusOrder = `orderdesc: status_user_emoji_set_at`

// CurrentEmojiStatus is the edge every read takes a person's status by, with
// $time as now: of their statuses that haven't ended, the one set last. A
// status that ended later than a newer one once stood in its place, so a new
// "30 min" status gave way to last week's on the next load. Setting or
// clearing a status now ends every other one (SetUserEmojiStatus,
// EndUserEmojiStatuses), so this is also the only one in force; the order
// decides only where an older server left two.
const CurrentEmojiStatus = `user_emoji_statuses @filter(gt(status_user_emoji_expiry_at, $time)) (` + emojiStatusOrder + `, first: 1) {
						uid
						status_user_emoji_id
						status_user_emoji_desc
						status_user_emoji_expiry_at
						status_user_emoji_expiry_in
					}`

// statusOwner is the query block naming a person's node as `user`, or an
// error for an id that isn't a person's (it is written into the query).
func statusOwner(userUUID string) (string, error) {
	if _, err := uuid.Parse(userUUID); err != nil {
		return "", fmt.Errorf("not a person's id %q: %w", userUUID, err)
	}
	return "user as var(func: " + dgraphquery.PersonByUUID(strconv.Quote(userUUID)) + ")", nil
}

// SetUserEmojiStatus makes status the person's status, in one transaction:
// every status of theirs still in force ends at now, the new one is added,
// set at now, and the oldest beyond EmojiStatusesKept are deleted.
func SetUserEmojiStatus(ctx context.Context, userUUID string, status *dgraphStruct.DgraphUserStatusEmoji, now time.Time) error {
	owner, err := statusOwner(userUUID)
	if err != nil {
		return err
	}
	at := now.UTC().Format(time.RFC3339Nano)
	// The new one isn't in the graph yet, so the newest EmojiStatusesKept-1
	// stay beside it, and every other one goes: picked as not the newest,
	// unsorted, because a sorted block with an offset and no first stops at
	// 1,000 (PROJ-BUG-8), and someone with more kept the rest.
	query := fmt.Sprintf(`query {
		%s
		var(func: uid(user)) {
			newest as user_emoji_statuses (%s, first: %d)
		}
		var(func: uid(user)) {
			beyond as user_emoji_statuses @filter(NOT uid(newest))
		}
		var(func: uid(user)) {
			inForce as user_emoji_statuses @filter(gt(status_user_emoji_expiry_at, %q) AND NOT uid(beyond))
		}
	}`, owner, emojiStatusOrder, EmojiStatusesKept-1, at)

	added := *status
	added.Uid = "_:status"
	added.DType = []string{"Status"}
	setAt := now
	added.SetAt = &setAt
	set, err := json.Marshal([]any{
		map[string]any{"uid": "uid(inForce)", "status_user_emoji_expiry_at": at},
		map[string]any{"uid": "uid(user)", "user_emoji_statuses": []any{&added}},
	})
	if err != nil {
		return err
	}
	del, err := json.Marshal([]any{
		map[string]any{"uid": "uid(user)", "user_emoji_statuses": []any{map[string]string{"uid": "uid(beyond)"}}},
		map[string]string{"uid": "uid(beyond)"},
	})
	if err != nil {
		return err
	}
	if err = dgraphModels.UpsertEmojiStatuses(ctx, query, set, del); err != nil {
		return err
	}
	forgetProfile(ctx, userUUID)
	return nil
}

// EndUserEmojiStatuses clears a person's status: every status of theirs still
// in force ends at now, in one transaction. Clear once ended only the status
// that would end last, and an earlier one still in force took its place.
func EndUserEmojiStatuses(ctx context.Context, userUUID string, now time.Time) error {
	owner, err := statusOwner(userUUID)
	if err != nil {
		return err
	}
	at := now.UTC().Format(time.RFC3339Nano)
	query := fmt.Sprintf(`query {
		%s
		var(func: uid(user)) {
			inForce as user_emoji_statuses @filter(gt(status_user_emoji_expiry_at, %q))
		}
	}`, owner, at)
	set, err := json.Marshal([]any{map[string]any{"uid": "uid(inForce)", "status_user_emoji_expiry_at": at}})
	if err != nil {
		return err
	}
	if err = dgraphModels.UpsertEmojiStatuses(ctx, query, set, nil); err != nil {
		return err
	}
	forgetProfile(ctx, userUUID)
	return nil
}

// CreateOrUpdateDgraphUser writes a person's node by their uuid, creating it
// when there is none, and answers its uid when it was created. Every writer
// of a person's names comes through here: with them it writes their
// user_search_name (searchName.go), made from the names the write leaves
// stored, read in the same transaction, so a concurrent save of the same
// person either lands first and is read, or aborts this one, which runs
// again.
func CreateOrUpdateDgraphUser(ctx context.Context, dgraphUser *dgraphStruct.DgraphUser) (newUserUid string, err error) {

	dgraphUser.DType = []string{"User"}
	dgraphUser.Uid = "uid(user)"
	vars := map[string]string{"$id": dgraphUser.Uuid}

	err = dgexec.InTxn(ctx, func(tx *dgexec.Txn) error {
		stored, err := dgexec.ReadIn[people](tx, storedNamesQuery, vars)
		if err != nil {
			return err
		}
		was := &dgraphStruct.DgraphUser{}
		if len(stored.People) > 0 {
			was = &stored.People[0]
		}
		dgraphUser.SearchName = searchNameOf(dgraphUser, was)
		res, err := tx.Mutate(`query user($id: string) { user as var(func: `+dgraphquery.PersonByUUID("$id")+`) }`, vars, dgexec.Mutation{Set: dgraphUser})
		if err != nil {
			return err
		}
		newUserUid = res.GetUids()["uid(user)"]
		return nil
	})

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/CreateOrUpdateDgraphUser Failed to create user in dgraph err: %+v",
			err,
		)
		return
	}

	forgetProfile(ctx, dgraphUser.Uuid)

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
					user_posts @filter(` + dgraphquery.Live("post_deleted_at") + `) (orderdesc: post_created_at, first: $first, offset: $offset) @cascade(post_channel){
						post_uuid
						post_text
						post_created_at
						post_by {
							user_uuid
							user_name
							user_profile_object_key
							user_full_name
						}
						post_comment_count : count(post_comments @filter(` + dgraphquery.Live("comment_deleted_at") + `))
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
					user_projects @filter(` + dgraphquery.Deleted("project_deleted_at") + `) { archivedProjects as uid }
				}
				userInfo(func: eq(user_uuid, $id)) @filter(` + dgraphquery.Live("user_deleted_at") + `) {
					uid
					user_uuid
					user_name
					user_full_name
					user_email_id
					user_profile_object_key
					user_job_title
					user_hobbies
					user_device_connected
					user_status
					` + CurrentEmojiStatus + `
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
			user_task_count: count(user_tasks @filter(` + dgraphquery.Live("task_deleted_at") + ` AND not uid_in(task_project, uid(archivedProjects)) @@hidden_tasks@@))
			user_incomplete_task_count: count(user_tasks @filter(` + dgraphquery.Live("task_deleted_at") + ` AND not uid_in(task_project, uid(archivedProjects)) AND ` + dgraphStruct.TASK_OPEN_FILTER + ` AND not eq(task_status, "canceled") @@hidden_tasks@@))
			user_overdue_task_count: count(user_tasks @filter(` + dgraphquery.Live("task_deleted_at") + ` AND not uid_in(task_project, uid(archivedProjects)) AND ` + dgraphStruct.TASK_OPEN_FILTER + ` AND not eq(task_status, "canceled") AND lt(task_due_date, $time) AND ` + dgraphquery.Deleted("task_due_date") + ` @@hidden_tasks@@))
			user_created_at
			user_updated_at
			user_deleted_at
			user_app_lang
			user_theme_color
			user_theme_mode
		}
	}`

	// The counts are of the person's own tasks, which they are always let into
	// (their assignee): for them, and so for anyone, the filter leaves none
	// out, but the counts ask it all the same (models/taskread).
	countFilter, err := taskread.For(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	query = countFilter.Fill(query)

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
	forgetProfile(ctx, userUUID)
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
					user_emoji_statuses @filter(gt(status_user_emoji_expiry_at, $time)) {
						status_user_emoji_id
						status_user_emoji_desc
						status_user_emoji_expiry_at
						status_user_emoji_expiry_in
						status_user_emoji_set_at
					}
				}
			}`

	dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query, variables)
	if err == nil && dgraphUser != nil {
		// Set last first, as emojiStatusOrder orders them (one set before set
		// times were kept goes last), sorted here: Dgraph stops a sorted block
		// at 1,000.
		slices.SortStableFunc(dgraphUser.StatusEmoji, func(a, b *dgraphStruct.DgraphUserStatusEmoji) int {
			return helpers.CompareTimes(a.SetAt, b.SetAt, true)
		})
	}

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserInfoByUUIDForSidebarNav Failed to get user in dgraph err: %+v",
			err,
		)
		return
	}

	return
}

// userTaskCardFields is a card on a person's board (My Tasks). It reads
// $userDgraphUID, for whether they may change the task's project.
var userTaskCardFields = `
						task_uuid
						id: task_uuid
						task_name
						task_status
						task_custom_status
						task_custom_status_name
						task_due_date
						task_start_date
						task_label
						task_priority
						task_project {
							uid
							project_uuid
							project_name
							project_is_admin: count(project_admins @filter(uid($userDgraphUID)))
							project_is_member: count(project_members @filter(uid($userDgraphUID)))
						}
						task_assignee {
							uid
							user_uuid
							user_name
							user_profile_object_key
						}
						task_sub_task_count: count(task_sub_tasks @filter(` + dgraphquery.Live("task_deleted_at") + ` @@hidden_tasks@@))
						task_comment_count: count(task_comments @filter(` + dgraphquery.Live("comment_deleted_at") + `))
						` + dgraphStruct.TASK_BLOCKED_OPEN + `
						task_team {
							team_name
							team_uuid
						}
						task_created_at
						task_rank
						task_status_since`

// GetDgraphUserBoardPlaces reads a person's board (My Tasks) without its
// cards, in txn: their tasks' places by column (dgraphStruct.BoardPlacesQuery),
// leaving out tasks of archived projects. The cards come from
// GetDgraphUserBoardCards, in the same snapshot.
func GetDgraphUserBoardPlaces(ctx context.Context, txn *dgo.Txn, userUUID string, filterQuery string, closedLimit int) (*dgraphStruct.DgraphUser, map[string]*dgraphStruct.BoardPlaces, error) {
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, nil, err
	}
	filter := dgraphStruct.TASK_LIVE_FILTER + " AND not uid_in(task_project, uid(archivedProjects))"
	if len(filterQuery) > 0 {
		filter += " AND " + filterQuery
	}
	// A person's own tasks are theirs to see; anyone else's board leaves out
	// the restricted ones the reader may not (models/taskread).
	filter += tr.And()
	query := `query UserInfo($id: string){
				# Tasks of archived projects are not the user's work any more.
				var(func: eq(user_uuid, $id)) {
					user_projects @filter(` + dgraphquery.Deleted("project_deleted_at") + `) { archivedProjects as uid }
				}
				userInfo(func: eq(user_uuid, $id)) {
					user_uuid` + dgraphStruct.BoardPlacesQuery("user_tasks", filter, closedLimit) + `
				}
			}`
	var out struct {
		UserInfo []json.RawMessage `json:"userInfo"`
	}
	if err := dgraphInit.ReadIntoTxn(ctx, txn, query, map[string]string{"$id": userUUID}, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphUserBoardPlaces Failed to get the user's board from dgraph err: %+v", err)
		return nil, nil, err
	}
	if len(out.UserInfo) == 0 {
		return nil, nil, errors.New("failed to get dgraph user")
	}
	var user dgraphStruct.DgraphUser
	if err := json.Unmarshal(out.UserInfo[0], &user); err != nil {
		return nil, nil, err
	}
	places, err := dgraphStruct.ParseBoardPlaces(out.UserInfo[0])
	if err != nil {
		return nil, nil, err
	}
	return &user, places, nil
}

// GetDgraphUserBoardCards reads the cards of a person's board by uid, in txn,
// in no particular order; userDgraphUID is the person, for whether they may
// change each task's project.
func GetDgraphUserBoardCards(ctx context.Context, txn *dgo.Txn, userDgraphUID string, uids []string) ([]*dgraphStruct.DgraphTask, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	if !dgraphquery.AllUIDs(uids) {
		return nil, errors.New("domain/GetDgraphUserBoardCards: not a list of task ids")
	}
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	query := tr.Fill(fmt.Sprintf(`query Cards($userDgraphUID: string){
				cards(func: uid(%s)) {%s
				}
			}`, strings.Join(uids, ", "), userTaskCardFields))
	return dgraphTaskModels.QueryDgraphTasksInTxn(ctx, txn, query, map[string]string{"$userDgraphUID": userDgraphUID})
}

func GetDgraphUserTaskList(ctx context.Context, userUUID string, userDgraphUID string, filterQuery string, sortQuery string, pageSize int, pageIndex int, getAll bool) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	if len(filterQuery) > 0 {
		filterQuery = "AND " + filterQuery
	}
	// A person's own tasks are theirs to see; anyone else's list, its count
	// and its sub-task counts leave out the restricted ones the reader may not
	// (models/taskread).
	tr, err := taskread.From(ctx)
	if err != nil {
		return nil, err
	}
	variables := make(map[string]string)
	variables["$id"] = userUUID
	variables["$userDgraphUID"] = userDgraphUID
	order := dgraphquery.TotalOrder(sortQuery, "task_uuid")

	query := func(page dgraphquery.Page) string {
		return tr.Fill(fmt.Sprintf(`query UserInfo($id: string, $userDgraphUID: string){
				# Tasks of archived projects are not the user's work any more.
				var(func: eq(user_uuid, $id)) {
					user_projects @filter(`+dgraphquery.Deleted("project_deleted_at")+`) { archivedProjects as uid }
				}
				userInfo(func: eq(user_uuid, $id)) {
					user_task_count: count(user_tasks @filter(`+dgraphquery.Live("task_deleted_at")+` AND not uid_in(task_project, uid(archivedProjects)) %s @@hidden_tasks@@))
					user_tasks @filter(`+dgraphquery.Live("task_deleted_at")+` AND not uid_in(task_project, uid(archivedProjects)) %s @@hidden_tasks@@) (%s, %s) {
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
							project_is_member: count(project_members @filter(uid($userDgraphUID)))
						}
						task_assignee {
							uid
							user_uuid
							user_name
							user_profile_object_key
						}
						task_sub_task_count: count(task_sub_tasks @filter(`+dgraphquery.Live("task_deleted_at")+` @@hidden_tasks@@))
						task_comment_count: count(task_comments @filter(`+dgraphquery.Live("comment_deleted_at")+`))
						`+dgraphStruct.TASK_BLOCKED_OPEN+`
						task_team {
							team_name
							team_uuid
						}
						task_created_at
					}
				}
			}`, filterQuery, filterQuery, order, page.Args()))
	}

	if !getAll && pageSize > 0 {
		dgraphUser, err = dgraphModels.GetDgraphUserInfoByUUID(ctx, query(dgraphquery.Page{First: pageSize, Offset: pageIndex * pageSize}), variables)
	} else {
		// Everything: one sorted block stops at 1,000, so page through it, in
		// one snapshot so the pages meet.
		err = dgraphInit.InSnapshot(ctx, func(txn *dgo.Txn) error {
			tasks, err := dgraphquery.ReadAll(dgraphquery.ReadAllPage, func(page dgraphquery.Page) ([]*dgraphStruct.DgraphTask, error) {
				got, err := dgraphModels.GetDgraphUserInfoInTxn(ctx, txn, query(page), variables)
				if err != nil {
					return nil, err
				}
				if dgraphUser == nil {
					dgraphUser = got
				}
				return got.Tasks, nil
			})
			if err == nil {
				dgraphUser.Tasks = tasks
			}
			return err
		})
	}

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/GetDgraphUserTaskList Failed to get user's task list from dgraph err: %+v",
			err,
		)
		return nil, err
	}
	return

}

func GetDgraphUserInfoByUUIDForSidebarNav(ctx context.Context, userUUID string) (dgraphUser *dgraphStruct.DgraphUser, err error) {
	variables := make(map[string]string)
	variables["$id"] = userUUID
	variables["$time"] = time.Now().Format(time.RFC3339Nano)

	query := `query UserInfo($id: string, $time: string){
				# A doc they made and handed on is the new owner's to name.
				sidebarMe as var(func: eq(user_uuid, $id))
				# Tasks of archived projects are not the user's work any more.
				var(func: eq(user_uuid, $id)) {
					user_projects @filter(` + dgraphquery.Deleted("project_deleted_at") + `) { archivedProjects as uid }
				}
				userInfo(func: eq(user_uuid, $id)) {
					uid
					user_uuid
					user_name
					user_email_id
					user_status
					user_profile_object_key
				# ch_private for the lock beside a private channel's name. The
				# sidebar lists channels by name, so it reads no channel's newest
				# post: that read sorted every post of every channel, and counted
				# deleted ones (F-33).
				user_channels @filter( ` + dgraphquery.Live("ch_deleted_at") + `){
					ch_uuid
					ch_name
					ch_private
				}
				# An archived channel leaves Starred as it leaves Channels.
				user_fav_channels @filter( ` + dgraphquery.Live("ch_deleted_at") + `){
					ch_uuid
					ch_name
					ch_private
				}
					user_teams {
						team_name
						team_uuid
					}
					` + CurrentEmojiStatus + `
					user_dms {
						dm_grouping_id
						dm_name
						dm_participants {
							user_uuid
							user_name
							user_full_name
							is_bot
							user_profile_object_key
							user_device_connected
							user_status
							` + CurrentEmojiStatus + `
						}
						# The newest live message: a deleted one moved its
						# conversation up as if someone had just written (F-33).
						dm_chats @filter(` + dgraphquery.Live("chat_deleted_at") + `) (orderdesc: chat_created_at, first: 1) {
							chat_created_at
						}
					}
				user_task_count: count(user_tasks @filter(` + dgraphquery.Live("task_deleted_at") + ` AND not uid_in(task_project, uid(archivedProjects)) @@hidden_tasks@@))
				user_incomplete_task_count: count(user_tasks @filter(` + dgraphquery.Live("task_deleted_at") + ` AND not uid_in(task_project, uid(archivedProjects)) AND ` + dgraphStruct.TASK_OPEN_FILTER + ` AND not eq(task_status, "canceled") @@hidden_tasks@@))
				user_overdue_task_count: count(user_tasks @filter(` + dgraphquery.Live("task_deleted_at") + ` AND not uid_in(task_project, uid(archivedProjects)) AND ` + dgraphStruct.TASK_OPEN_FILTER + ` AND not eq(task_status, "canceled") AND lt(task_due_date, $time) AND ` + dgraphquery.Deleted("task_due_date") + ` @@hidden_tasks@@))
				user_docs: ~doc_created_by(orderdesc: doc_updated_at, first: 8) @filter(` + dgraphquery.Live("doc_deleted_at") + ` AND (NOT has(doc_owner) OR uid_in(doc_owner, uid(sidebarMe)))) {
					doc_uuid
					doc_title
				}
				user_boards: ~board_created_by(orderdesc: board_updated_at, first: 8) @filter(` + dgraphquery.Live("board_deleted_at") + `) {
					board_uuid
					board_title
				}
				user_projects @filter(` + dgraphquery.Live("project_deleted_at") + `) {
					project_uuid
					project_name
				}
				user_theme_color
				user_theme_mode
			}
		}`

	// The counts are of the person's own tasks, which they are always let into
	// (their assignee): for them, and so for anyone, the filter leaves none
	// out, but the counts ask it all the same (models/taskread).
	countFilter, err := taskread.For(ctx, userUUID)
	if err != nil {
		return nil, err
	}
	query = countFilter.Fill(query)

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
					` + CurrentEmojiStatus + `
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
				userInfo(func: uid(%+v)) @filter(has(user_uuid)) {
					uid
					user_uuid
					user_name
					user_email_id
					is_external
					is_bot
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

// allUsersListMax is the most people GetDgraphAllUsersList reads: the
// @mention picker, the DM picker and the goal owner picker hold the whole list
// to search it, which a workspace far past this should page instead.
const allUsersListMax = 5000

// GetDgraphAllUsersList is the workspace's people (bots included, guests not)
// for the pickers that search them in the browser: what they show and search
// by, at most allUsersListMax. It also read every post each person had written
// (user_posts, a uid apiece), their channels, hobbies and timestamps, none of
// which a picker uses, so the list grew with the workspace's history.
func GetDgraphAllUsersList(ctx context.Context) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {
	return GetDgraphUsersFirst(ctx, allUsersListMax)
}

// GetDgraphUsersFirst is the first of the workspace's people (bots included,
// guests not), as GetDgraphAllUsersList reads them: what a picker shows and
// searches by, at most first.
func GetDgraphUsersFirst(ctx context.Context, first int) (dgraphUsers []*dgraphStruct.DgraphUser, err error) {

	variables := make(map[string]string)
	query := fmt.Sprintf(`query UserInfo(){

				userInfo(func: has(user_uuid), first: %d) @filter(NOT eq(is_external, true) OR eq(is_bot, true)) {
					uid
					user_uuid
					user_name
					user_full_name
					user_email_id
					user_profile_object_key
					user_job_title
					is_bot
					user_deleted_at
				}
			}`, first)

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

// MembersFilter narrows the admin's members list: Q is matched against the
// display name, full name and address (any case); Admins, when set, keeps
// only workspace admins (true) or only everyone else (false).
type MembersFilter struct {
	Q      string
	Admins *bool
}

// AdminUserIDs is the account ids of the workspace's admins (admin_users
// names them by address).
func AdminUserIDs(ctx context.Context) ([]string, error) {
	return pgexec.Query(ctx, `
		SELECT u.id FROM users u
		JOIN admin_users a ON LOWER(a.email_id) = LOWER(u.email_id)
		WHERE u.deleted_at IS NULL`, func(r pgexec.Row) (string, error) {
		var id uuid.UUID
		err := r.Scan(&id)
		return id.String(), err
	})
}

// GetMembersPage is a page of the members list, newest first, with how many
// match f in all: the admin's list searched and counted on the server, where
// the web app walked every page to search and count in the browser.
func GetMembersPage(ctx context.Context, first, offset int, f MembersFilter) ([]*dgraphStruct.DgraphUser, int, error) {
	filter := "NOT eq(is_external, true)"
	if q := escapeForDgraphRegex(f.Q); q != "" {
		filter += fmt.Sprintf(" AND (regexp(user_name, /.*%[1]s.*/i) OR regexp(user_full_name, /.*%[1]s.*/i) OR regexp(user_email_id, /.*%[1]s.*/i))", q)
	}
	if f.Admins != nil {
		admins, err := AdminUserIDs(ctx)
		if err != nil {
			return nil, 0, err
		}
		list := dgraphquery.UUIDList(admins)
		switch {
		case *f.Admins && list == "":
			return nil, 0, nil
		case *f.Admins:
			filter += " AND eq(user_uuid, [" + list + "])"
		case list != "":
			filter += " AND NOT eq(user_uuid, [" + list + "])"
		}
	}
	query := fmt.Sprintf(`query Members($first: int, $offset: int) {
		matched as var(func: has(user_uuid)) @filter(%s)
		total(func: uid(matched)) { count(uid) }
		userInfo(func: uid(matched), first: $first, offset: $offset, orderdesc: user_created_at) {
			user_uuid
			user_profile_object_key
			user_device_connected
			user_email_id
			user_status
			user_name
			user_full_name
			user_deleted_at
		}
	}`, filter)
	out, err := dgexec.Read[struct {
		Users []*dgraphStruct.DgraphUser `json:"userInfo"`
		Total []struct {
			Count int `json:"count"`
		} `json:"total"`
	}](ctx, query, map[string]string{"$first": strconv.Itoa(first), "$offset": strconv.Itoa(offset)})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetMembersPage err: %+v", err)
		return nil, 0, err
	}
	total := 0
	if len(out.Total) > 0 {
		total = out.Total[0].Count
	}
	return out.Users, total, nil
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
	userID, found, err := pgexec.QueryOne(ctx, `
		SELECT id FROM users
		WHERE LOWER(email_id) = LOWER($1) AND deleted_at IS NULL AND is_external = false AND is_bot = false
		`+sameAddressFirst, scanUUID, userEmailId)
	if err == nil && !found {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetDgraphUserInfoByEmailId Failed to find the member err: %+v", err)
		return nil, err
	}

	variables := make(map[string]string)
	variables["$id"] = userID.String()
	query := `query UserInfo($id: string){
				userInfo(func: eq(user_uuid, $id)) @filter(` + dgraphquery.Live("user_deleted_at") + ` AND NOT eq(is_external, true) AND NOT eq(is_bot, true)) {
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
						ch_recs as ch_recording @filter(ge(recording_stared_at, $startDate) AND le(recording_stared_at, $endDate) AND ` + dgraphquery.Live("recording_deleted_at") + ` AND (NOT eq(recording_transcript_only, true) OR ` + dgraphquery.Deleted("recording_ended_at") + `))
					}
					~dm_participants {
						dm_recs as dm_recording @filter(ge(recording_stared_at, $startDate) AND le(recording_stared_at, $endDate) AND ` + dgraphquery.Live("recording_deleted_at") + ` AND (NOT eq(recording_transcript_only, true) OR ` + dgraphquery.Deleted("recording_ended_at") + `))
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
					recording_transcript_only
					recording_status
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
	userInfo, err = one(ctx, `
		SELECT id, email_id, password_hash, username, created_at, updated_at,
		       COALESCE(is_sso_managed, false), signup_method, last_login_method
		FROM users
		WHERE LOWER(email_id) = LOWER($1)
		AND deleted_at IS NULL
		AND is_external = false
		AND is_bot = false
		`+sameAddressFirst, func(r pgexec.Row) (*models.User, error) {
		var user models.User
		err := r.Scan(&user.Id, &user.EmailID, &user.PasswordHash, &user.Username, &user.CreatedAt, &user.UpdatedAt,
			&user.IsSSOManaged, &user.SignupMethod, &user.LastLoginMethod)
		return &user, err
	}, emailID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetUserByEmailIdWithPassword Failed to get user err: %+v", err)
		return nil, err
	}
	return userInfo, nil
}

func UpdatePasswordByUserID(ctx context.Context, userID uuid.UUID, passwordHash *string) (err error) {
	if _, err = pgexec.Exec(ctx, `
		UPDATE users
		SET password_hash = $1, password_generated = false, updated_at = NOW()
		WHERE id = $2`, passwordHash, userID); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpdatePasswordByUserID Failed to update password err: %+v", err)
	}
	return
}

// MarkPasswordGenerated records that the account's password was generated for
// its owner rather than chosen by them. UpdatePasswordByUserID clears it.
func MarkPasswordGenerated(ctx context.Context, userID uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `UPDATE users SET password_generated = true WHERE id = $1`, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MarkPasswordGenerated err: %+v", err)
	}
	return err
}

// PasswordGenerated reports whether the account still has a generated password.
func PasswordGenerated(ctx context.Context, userID uuid.UUID) (bool, error) {
	return pgexec.Scalar[bool](ctx, `SELECT password_generated FROM users WHERE id = $1`, userID)
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
	// The request's values (its id, for the logs), not its cancellation: a
	// sign-up the client walked away from still writes both stores, or the
	// graph node and the row disagree.
	_, err = pgexec.Exec(context.WithoutCancel(ctx), query, userUUID, emailID, username, passwordHash, signupMethod, isSSOManaged)
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
	// Not cancelled with the request: the row stays locked until both stores
	// agree, and a half-finished adoption would leave them disagreeing.
	graphMarked := false
	err = pgexec.InTx(context.WithoutCancel(ctx), func(tx context.Context) error {
		id, found, err := pgexec.QueryOne(tx, `
			SELECT id FROM users
			WHERE LOWER(email_id) = LOWER($1) AND is_external = true AND is_bot = false AND deleted_at IS NULL
			`+sameAddressFirst+`
			FOR UPDATE`, scanUUID, emailID)
		if err != nil || !found {
			return err
		}
		if err := EnsureSeatAvailable(ctx); err != nil {
			return err
		}
		if _, err := pgexec.Exec(tx, `
			UPDATE users
			SET is_external = false, password_hash = $2, signup_method = NULLIF($3, ''),
				is_sso_managed = $4, updated_at = NOW()
			WHERE id = $1`, id, passwordHash, signupMethod, isSSOManaged); err != nil {
			return err
		}
		if err := setDgraphExternal(tx, id.String(), false); err != nil {
			return err
		}
		graphMarked, userID = true, id
		return nil
	})
	if err != nil {
		// Only the commit fails after the graph node is marked a member's.
		if graphMarked {
			_ = helpers.CompensateOnFailure(ctx, "the member mark on the graph node of an adopted "+emailID,
				func(undoCtx context.Context) error { return setDgraphExternal(undoCtx, userID.String(), true) })
		}
		return uuid.Nil, false, err
	}
	if userID == uuid.Nil {
		return uuid.Nil, false, nil
	}
	forgetProfile(ctx, userID.String())
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
		Query:     `query q($id: string) { found(func: ` + dgraphquery.PersonByUUID("$id") + `) { u as uid } }`,
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
	_, err := pgexec.Exec(ctx, `
		UPDATE users
		SET last_login_method = $1, last_login_at = NOW(), updated_at = NOW()
		WHERE id = $2`, method, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/RecordLoginMethod Failed to record login method err: %+v", err)
	}
	return err
}

// GetUserAuthFlagsByUUID loads only the auth-related flags + email for a user
// by UUID. Cheaper than the full GetUserByUUID and includes is_sso_managed
// which the legacy loader doesn't.
func GetUserAuthFlagsByUUID(ctx context.Context, userID uuid.UUID) (*models.User, error) {
	user, err := one(ctx, `
		SELECT id, email_id, COALESCE(is_sso_managed, false), signup_method, last_login_method
		FROM users
		WHERE id = $1
		AND deleted_at IS NULL`, func(r pgexec.Row) (*models.User, error) {
		var user models.User
		err := r.Scan(&user.Id, &user.EmailID, &user.IsSSOManaged, &user.SignupMethod, &user.LastLoginMethod)
		return &user, err
	}, userID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetUserAuthFlagsByUUID err: %+v", err)
		return nil, err
	}
	return user, nil
}

// GetInvitationByLink is the invitation a link's hash finds
// (helpers.LinkSecretHash), or sql.ErrNoRows.
func GetInvitationByLink(ctx context.Context, hash []byte) (invitation *models.Invitation, err error) {
	invitation, err = one(ctx, `
		SELECT `+invitationColumns+`
		FROM invitations
		WHERE token_hash = $1`, scanInvitation, hash)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetInvitationByLink Failed to get invitation err: %+v", err)
		return nil, err
	}
	return invitation, nil
}

func UpdateInvitationStatus(ctx context.Context, invitationID uuid.UUID, status string) (err error) {
	if _, err = pgexec.Exec(ctx, `UPDATE invitations SET status = $1 WHERE id = $2`, status, invitationID); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/UpdateInvitationStatus Failed to update invitation status err: %+v", err)
	}
	return
}

func AddInvitationWithLink(ctx context.Context, email string, invitedBy uuid.UUID, link models.InvitationLink, expiresAt time.Time) (err error) {
	if _, err = pgexec.Exec(ctx, `
		INSERT INTO invitations (email, invited_by, status, token_hash, token_sealed, token_expires_at)
		VALUES ($1, $2, 'sent', $3, $4, $5)`, email, invitedBy, link.Hash, link.Sealed, expiresAt); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/AddInvitationWithLink Failed to add invitation err: %+v", err)
	}
	return
}

func CheckIfAnyAdminExists(ctx context.Context) (exists bool, err error) {
	exists, err = pgexec.Scalar[bool](ctx, `SELECT EXISTS (SELECT 1 FROM admin_users LIMIT 1)`)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CheckIfAnyAdminExists Failed to check admin existence err: %+v", err)
	}
	return
}

// UpdateInvitationLinkByID gives one invitation a new link and expiry, from
// invitedBy, who renewed it: the invitation is theirs now, and its email
// names them.
//
// By id, not by address: an address can have more than one invitation row
// (made before inviting a member twice was refused), and setting every one of
// them to the same new link broke on its unique index, so sending such an
// invitation again failed. The caller renews the newest
// (GetInvitationByEmail).
func UpdateInvitationLinkByID(ctx context.Context, id uuid.UUID, link models.InvitationLink, expiresAt time.Time, invitedBy uuid.UUID) error {
	_, err := pgexec.Exec(ctx, `
		UPDATE invitations
		SET token_hash = $1, token_sealed = $2, token_expires_at = $3, status = 'sent', invited_by = $5
		WHERE id = $4`, link.Hash, link.Sealed, expiresAt, id, invitedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"domain/UpdateInvitationLinkByID Failed to update invitation link err: %+v", err)
	}
	return err
}

// GetInvitationByEmail is the newest invitation for an address, the one that
// is renewed and judged, or nil when there is none.
func GetInvitationByEmail(ctx context.Context, email string) (*models.Invitation, error) {
	inv, found, err := pgexec.QueryOne(ctx, `
		SELECT `+invitationColumns+`
		FROM invitations
		WHERE LOWER(email) = LOWER($1)
		ORDER BY created_at DESC
		LIMIT 1`, scanInvitation, email)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/GetInvitationByEmail err: %+v", err)
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return inv, nil
}

// HasUsableInvitation reports whether email holds an invitation that can
// still let someone in, as models.Invitation.LiveAt decides: the rows are
// read and judged by that one predicate, not by a copy of it in SQL.
func HasUsableInvitation(ctx context.Context, email string) (bool, error) {
	invs, err := pgexec.Query(ctx, `
		SELECT `+invitationColumns+`
		FROM invitations
		WHERE LOWER(email) = LOWER($1)`, scanInvitation, email)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/HasUsableInvitation err: %+v", err)
		return false, err
	}
	now := time.Now()
	for _, inv := range invs {
		if inv.LiveAt(now) {
			return true, nil
		}
	}
	return false, nil
}

// MarkInvitationJoined records that the person invited at email has joined,
// however they came in, and says whether there was an invitation to mark.
// Nothing to mark is not an error.
func MarkInvitationJoined(ctx context.Context, email string) (marked bool, err error) {
	n, err := pgexec.Exec(ctx, `
		UPDATE invitations SET status = 'joined'
		WHERE LOWER(email) = LOWER($1) AND status <> 'joined'`, email)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MarkInvitationJoined err: %+v", err)
	}
	return n > 0, err
}

// MemberAccountState reports whether a member's account has this address,
// and whether it is deactivated. External rows and bots are not members.
func MemberAccountState(ctx context.Context, email string) (exists bool, deactivated bool, err error) {
	deactivated, exists, err = pgexec.QueryOne(ctx, `
		SELECT deleted_at IS NOT NULL FROM users
		WHERE LOWER(email_id) = LOWER($1) AND is_external = false AND is_bot = false
		ORDER BY (deleted_at IS NULL) DESC, created_at ASC
		LIMIT 1`, func(r pgexec.Row) (d bool, err error) { err = r.Scan(&d); return }, email)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/MemberAccountState err: %+v", err)
		return false, false, err
	}
	return exists, deactivated, nil
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
	// queries for `eq(field, [a, b, c])`, so the list is written into the
	// query: uuids only, lower case, as user_uuid keeps them.
	list := dgraphquery.UUIDList(clean)
	if list == "" {
		return nil, nil
	}

	query := fmt.Sprintf(`{
		userInfo(func: eq(user_uuid, [%s])) @filter(`+dgraphquery.Live("user_deleted_at")+`) {
			uid
			user_uuid
			user_name
			user_full_name
			user_email_id
			user_profile_object_key
			is_bot
			user_device_connected
			user_status
		}
	}`, list)

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
	//
	// It starts from the people whose search name matches when the term is
	// long enough for its index (searchName.go), not from every person; the
	// filter below still decides who matches. Not from a trigram index on
	// user_name: that is @upsert, and the index made each trigram a conflict
	// key, so people joining at once with like names aborted each other's
	// sign-up (c925fc70).
	vars, root := dgraphquery.RegexpUnionRoot(safe, "has(user_uuid)", "people", searchNamePredicates)
	query := fmt.Sprintf(`{
			%s
			userInfo(func: %s, first: %d) @filter(has(user_uuid) AND
				(regexp(user_name, /.*%s.*/i)
				 OR regexp(user_full_name, /.*%s.*/i)
				 OR regexp(user_job_title, /.*%s.*/i)
				 OR regexp(user_department, /.*%s.*/i))
				AND NOT eq(is_external, true)
				AND NOT eq(is_bot, true)) {
				user_uuid
				user_name
				user_full_name
				user_job_title
				user_department
				user_email_id
			}
			}`, vars, root, limit, safe, safe, safe, safe)

	dgraphUsers, err = dgraphModels.GetDgraphUsersList(ctx, query, map[string]string{})
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/SearchPeople failed err: %+v", err)
		return nil, err
	}
	return dgraphUsers, nil
}

// searchNamePredicates is where the people searches start: user_search_name
// holds every name they match (searchName.go).
var searchNamePredicates = []string{"user_search_name"}

// peopleSearchMaxResults bounds what a single lookup returns. A directory is not
// something an agent should be able to page through one tool call at a time.
const peopleSearchMaxResults = 15

// IsActiveAdmin reports whether the account id is active and a workspace admin
// now. No id, a deleted account or a failed lookup is false, so a rule that
// grants admins more fails closed.
func IsActiveAdmin(ctx context.Context, id *uuid.UUID) bool {
	if id == nil || *id == uuid.Nil {
		return false
	}
	u, err := GetActiveUserWithAdminFlagByUserUUID(ctx, *id)
	return err == nil && u != nil && u.Id == *id && u.IsAdmin
}
