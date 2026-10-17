// Who edited a doc since its last save: the attribution a save carries to the
// API, where it goes into the doc's contributor ledger and from there into the
// next version of its history (business/Snapshot).
//
// A Redis set per document, so it works across instances. Someone typing over a
// socket is recorded by their user id (collabServer.js onChange). A write through
// the Live Doc Writer (live/ops) records who the API says it is from, never what
// the client claims (I11): a person by their id, an agent as JSON with the person
// who asked it.
//
// A save reads the set without clearing it and, once the API has the save,
// removes exactly the members it sent. Read-and-clear lost everyone whose edits
// were in a save that then failed.

export const CONTRIBUTORS_TTL_SECONDS = 60 * 60
export const contributorsKey = (documentName) => `collab:contributors:${documentName}`

// contributorMember is how one contributor is kept in the set: a person as
// their id, an agent as {"agent":…,"asker":…}. '' for nobody.
export function contributorMember({ user = '', agent = '' } = {}) {
    if (typeof agent === 'string' && agent.trim()) {
        return JSON.stringify({ agent: agent.trim(), asker: typeof user === 'string' ? user.trim() : '' })
    }
    return typeof user === 'string' ? user.trim() : ''
}

// splitContributors turns the set's members into what a save sends: the people
// who edited, and the agents with whoever asked each. "unknown" is a server-side
// connection with nobody behind it (a save retry), never a person.
export function splitContributors(members) {
    const users = new Set()
    const agents = new Map()
    for (const m of Array.isArray(members) ? members : []) {
        if (typeof m !== 'string' || m === '' || m === 'unknown') continue
        if (m.startsWith('{')) {
            try {
                const { agent, asker } = JSON.parse(m)
                if (typeof agent === 'string' && agent) {
                    const a = { agent_uuid: agent, asker_uuid: typeof asker === 'string' ? asker : '' }
                    agents.set(`${a.agent_uuid}|${a.asker_uuid}`, a)
                }
            } catch {
                // not one of ours: skipped
            }
            continue
        }
        users.add(m)
    }
    return { users: [...users].sort(), agents: [...agents.values()] }
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
