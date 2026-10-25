// Asking again who may do what in a doc or a board (invariant I10).
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

// closeSockets closes each socket once (one socket carries every doc and board
// a tab has open) and answers how many it closed.
function closeSockets(sockets) {
    let closed = 0
    for (const socket of sockets) {
        try {
            socket.close(REAUTH_CLOSE.code, REAUTH_CLOSE.reason)
            closed++
        } catch {
            // already gone
        }
    }
    return closed
}

// named is the people a body names under key, blanks left out.
function named(body, key) {
    return new Set(Array.isArray(body?.[key]) ? body[key].filter((u) => typeof u === 'string' && u) : [])
}

// reauth closes the sockets on one doc or board (docName is board:<uuid> for a
// board) of the people named, or everyone's.
export async function reauth({ docName, body, hocuspocus }) {
    const users = named(body, 'user_uuids')
    const doc = hocuspocus.documents?.get(docName.toLowerCase())
    if (!doc) return { closed: 0 }
    const sockets = new Set()
    for (const connection of doc.getConnections()) {
        if (!users.size || users.has(connection.context?.user?.id)) sockets.add(connection.webSocket)
    }
    return { closed: closeSockets(sockets) }
}

// closePeople closes every socket the people named have open, on every doc
// and board: someone deactivated or taken off a team or project (their uuid),
// a guest whose link was turned off (guest-<grant id>). Their tabs reconnect
// and are authorised afresh, so the API refuses them. Naming nobody closes
// nothing: unlike reauth, it never means everyone.
export async function closePeople({ body, hocuspocus }) {
    const people = named(body, 'user_ids')
    if (!people.size) return { closed: 0 }
    const sockets = new Set()
    for (const doc of hocuspocus.documents?.values() ?? []) {
        for (const connection of doc.getConnections()) {
            if (people.has(connection.context?.user?.id)) sockets.add(connection.webSocket)
        }
    }
    return { closed: closeSockets(sockets) }
}

// The longest one timer waits (setTimeout's limit, about 24.8 days): a link
// that ends later is looked at again then.
const LONGEST_WAIT = 2 ** 31 - 1

// closeWhenLinkEnds closes a guest's socket when their link stops working
// (endsAt, ms since the epoch, from the API's answer to their authorisation),
// so an open view ends with the link and the reconnect is refused. A
// connection that closes first takes its timer with it.
export function closeWhenLinkEnds(connection, endsAt) {
    let timer
    const wait = () => {
        const left = endsAt - Date.now()
        timer = setTimeout(left > LONGEST_WAIT ? wait : () => closeSockets([connection.webSocket]), Math.min(Math.max(left, 0), LONGEST_WAIT))
        timer.unref?.()
    }
    wait()
    connection.onClose(() => clearTimeout(timer))
}
