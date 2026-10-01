package models

import "log/slog"

// LogValue limits what a UserInfo contributes to a log record.
//
// WHY, with numbers. UserInfo is put into the request context by six middlewares and is listed in
// helpers.DefaultContextKeys, so it was attached as an attribute to EVERY log line the request
// produced — at every level, to stdout and over OTLP to the collector. It was attached whole.
//
// Whole is much larger than it looks. GetActiveDgraphUserInfoByUUID hydrates UserDgraphInfo with
// the user's every channel, every project, every team with its project ids, and every DM grouping
// including each participant's uuid, name and profile key. Serialised, that is:
//
//	brand-new user (1 channel, 0 DMs)      ~760 bytes per log line
//	typical user   (20 channels, 15 DMs)  ~5,500 bytes per log line
//	heavy user     (80 channels, 60 DMs) ~20,800 bytes per log line
//
// At a few hundred lines a minute the typical case is megabytes of one attribute, repeated,
// mostly unchanged, to a sink that is usually billed by volume.
//
// THE POINT OF LOGGING THE USER IS KEPT. Knowing which user a failure belongs to is essential and
// nothing here removes it — UUID is the identifier every other record joins on, and it is emitted
// on every line as before. What is dropped is everything that does not help answer "which user":
//
//   - email, username, display name and GitHub identity. These do not narrow a search that a UUID
//     has already narrowed to one row; they are the same cardinality. When a human-readable name
//     is wanted, one lookup gives it. Meanwhile they make every log line personal data, with the
//     retention and access consequences that follow — logs typically outlive the database and are
//     readable by people who cannot read the database.
//   - the channel, project, team and DM graph. None of it describes the request being logged.
//   - and, most importantly, OTHER PEOPLE. The DM participants carried names and profile keys, so
//     a log line about Alice's failed request also disclosed the names of everyone Alice has a DM
//     with. Third-party data in a record about someone else is the part that has no defence.
//
// The three flags are kept because they change behaviour and therefore change triage: whether a
// request came from an admin, an external user, or a bot is usually the first question asked.
//
// Implemented as slog.LogValuer on the type rather than by narrowing the attribute at the log
// site. slog resolves LogValuer in the handler, so this applies to every existing log call and to
// every future one automatically. A separate "log-safe" context key would have had to be set by
// all six middlewares, and forgotten by the seventh — which is the shape of half the defects fixed
// in this codebase this week.
func (u UserInfo) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("uuid", u.UserPostgresInfo.Id.String()),
		slog.Bool("is_admin", u.UserPostgresInfo.IsAdmin),
		slog.Bool("is_external", u.UserPostgresInfo.IsExternal),
		slog.Bool("is_bot", u.UserPostgresInfo.IsBot),
	)
}
