package models

// MemberRecordingsBlock is the graph's one rule for the recordings a person
// reaches by membership, now: those of every channel they are a member of
// (~ch_members) and every chat they are in (~dm_participants). root names
// the person (uid($user_uid), or PersonByUUID), and recFilter is the caller's
// filter on the recordings. It sets two query variables, ch_recs and dm_recs.
// The recordings list and the transcript search's reach both read through
// it, so who may see a call's recording and who may find its words are the
// same people. It lives here, beside the queries that run it, so domain/User
// reads it without importing domain/Recording: the integration harness links
// domain/User, and a package it links can't test through it from inside.
func MemberRecordingsBlock(root, recFilter string) string {
	return `var(func: ` + root + `) {
					~ch_members {
						ch_recs as ch_recording @filter(` + recFilter + `)
					}
					~dm_participants {
						dm_recs as dm_recording @filter(` + recFilter + `)
					}
				}`
}
