// Who edited a doc since its last save: the attribution a save carries to the
// API, where it goes into the doc's contributor ledger and from there into the
// next version of its history (business/Snapshot).
//
// A Redis set per document, so it works across instances. Someone typing over a
// socket is recorded by their user id (collabServer.js onChange). A write through
// the Live Doc Writer (live/ops) records who the API says it is from, never what
// the client claims (I11): a person by their id, an agent as JSON with the person
// who asked it. A quiet write's (a copy's body, an import's) is marked quiet, and
// the save holding it says so: the API keeps whom the body mentions and tells
// nobody (business/Doc whoToTell).
//
// A QUIET MARK STAYS UNTIL A SAVE TAKES IT, and that save is quiet whatever else
// it holds. A quiet write that is refused takes its mark back (live/ops/write.js).
// In the running service the mark's save is the quiet write's own: the store
// saves the doc as that write left it, alone, and what anyone typed meanwhile
// goes in the next save, loudly, whether the quiet save went through at once or
// on a retry (collabServer.js, "A quiet write's own save").
// WHERE THE CONSERVATIVE CHOICE REMAINS: the doc is let go before that save
// went through, so the doc as the write left it, held in memory, is gone. That
// is a restart while the save is still failing (the mark is here with the
// doc's kept state, so the retry after the restart is quiet whatever it holds,
// and a mention typed into the doc before the restart tells nobody:
// live/ops.test.js, "after a restart"; N1-GAPS.md), or a save refused until the
// doc's unsaved edits were quarantined (the next save, within the mark's hour,
// is quiet too).
//
// A save reads the set without clearing it and, once the API has the save,
// removes exactly the members it sent. Read-and-clear lost everyone whose edits
// were in a save that then failed.

export const CONTRIBUTORS_TTL_SECONDS = 60 * 60
export const contributorsKey = (documentName) => `collab:contributors:${documentName}`

// contributorMember is how one contributor is kept in the set: a person as
// their id, an agent as {"agent":…,"asker":…}. A quiet write's is JSON marked
// quiet: a person as {"user":…,"quiet":true}, an agent as
// {"agent":…,"asker":…,"quiet":true}. '' for nobody, quiet or not.
export function contributorMember({ user = '', agent = '', quiet = false } = {}) {
    const person = typeof user === 'string' ? user.trim() : ''
    if (typeof agent === 'string' && agent.trim()) {
        return JSON.stringify(quiet === true ? { agent: agent.trim(), asker: person, quiet: true } : { agent: agent.trim(), asker: person })
    }
    if (quiet === true && person && person !== 'unknown') return JSON.stringify({ user: person, quiet: true })
    return person
}

// splitContributors turns the set's members into what a save sends: the people
// who edited, the agents with whoever asked each, and whether the save holds a
// quiet write. One quiet member makes the whole save quiet, and a quiet person
// still counts among the people who edited. "unknown" is a server-side
// connection with nobody behind it (a save retry), never a person.
export function splitContributors(members) {
    const users = new Set()
    const agents = new Map()
    let quiet = false
    for (const m of Array.isArray(members) ? members : []) {
        if (typeof m !== 'string' || m === '' || m === 'unknown') continue
        if (!m.startsWith('{')) {
            users.add(m)
            continue
        }
        let member = null
        try {
            member = JSON.parse(m)
        } catch {
            // not one of ours: skipped below
        }
        const { agent, asker, user } = member && typeof member === 'object' ? member : {}
        if (typeof agent === 'string' && agent) {
            const a = { agent_uuid: agent, asker_uuid: typeof asker === 'string' ? asker : '' }
            agents.set(`${a.agent_uuid}|${a.asker_uuid}`, a)
        } else if (typeof user === 'string' && user && user !== 'unknown') {
            users.add(user)
        } else {
            continue // not one of ours
        }
        if (member.quiet === true) quiet = true
    }
    return { users: [...users].sort(), agents: [...agents.values()], quiet }
}

// createContributors is the set for one Redis client. Every call is
// best-effort: attribution never stops an edit or a save.
export function createContributors(redis, { log = console } = {}) {
    return {
        async record(documentName, who) {
            const member = contributorMember(who)
            if (!member || member === 'unknown') return
            try {
                const key = contributorsKey(documentName)
                await redis.sadd(key, member)
                await redis.expire(key, CONTRIBUTORS_TTL_SECONDS)
            } catch (e) {
                log.error(`[Collab] Could not record a contributor to ${documentName}:`, e?.message || e)
            }
        },
        // peek is the set as it is now, members and all, for one save.
        async peek(documentName) {
            try {
                const members = await redis.smembers(contributorsKey(documentName))
                return Array.isArray(members) ? members : []
            } catch (e) {
                log.error(`[Collab] Failed to read contributors for ${documentName}:`, e?.message || e)
                return []
            }
        },
        // ack removes the members a save carried, once the API has it; anyone who
        // edited during the save stays for the next one.
        async ack(documentName, members) {
            if (!Array.isArray(members) || members.length === 0) return
            try {
                await redis.srem(contributorsKey(documentName), ...members)
            } catch (e) {
                log.error(`[Collab] Failed to clear saved contributors for ${documentName}:`, e?.message || e)
            }
        },
    }
}
