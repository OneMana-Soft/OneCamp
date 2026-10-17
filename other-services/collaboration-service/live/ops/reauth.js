// Asking again who may do what in a doc (invariant I10).
//
// A grant, a revoke, a lock or a downgrade changes what someone may do, but a
// socket is authorised once, when it connects. reauth closes the sockets of the
// people named (everyone's, when none are), and their editors reconnect at once
// and are authorised afresh: a person who lost access is refused, one who lost
// edit access comes back read-only. Unsent edits stay in the browser and are
// sent on the reconnect if the person may still write.
//
// The whole socket closes, not just this doc on it: Hocuspocus's per-document
// close message leaves a browser's provider detached and never reconnects it.

export const REAUTH_CLOSE = { code: 4205, reason: 'reauth' }

export async function reauth({ docName, body, hocuspocus }) {
    const users = Array.isArray(body?.user_uuids) ? new Set(body.user_uuids.filter((u) => typeof u === 'string' && u)) : new Set()
    const doc = hocuspocus.documents?.get(docName.toLowerCase())
    if (!doc) return { closed: 0 }
    let closed = 0
    for (const connection of doc.getConnections()) {
        const who = connection.context?.user?.id
        if (users.size && !users.has(who)) continue
        try {
            connection.webSocket.close(REAUTH_CLOSE.code, REAUTH_CLOSE.reason)
            closed++
        } catch {
            // already gone
        }
    }
    return { closed }
}
