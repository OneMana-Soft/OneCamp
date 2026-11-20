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
    rememberClosed([...people].filter((id) => id.startsWith('guest-')), Date.now())
    const sockets = new Set()
    for (const doc of hocuspocus.documents?.values() ?? []) {
        for (const connection of doc.getConnections()) {
            if (people.has(connection.context?.user?.id)) sockets.add(connection.webSocket)
        }
    }
    return { closed: closeSockets(sockets) }
}

// When the close route last named each guest (guest-<grant id> -> ms): a
// link turned off, or changed (its role, its password). An op on a socket
// opened before then is refused even if it races the close, while the tab's
// reconnect, authorised afresh, writes as its link now lets it. Kept for an
// hour (a guest's live token lasts 30 minutes), at most MAX_CLOSED entries.
const closedAt = new Map()
const CLOSED_FOR = 60 * 60 * 1000
const MAX_CLOSED = 10000

function rememberClosed(ids, now) {
    for (const [id, at] of closedAt) {
        if (now - at < CLOSED_FOR && closedAt.size < MAX_CLOSED) break
        closedAt.delete(id)
    }
    for (const id of ids) {
        closedAt.delete(id)
        closedAt.set(id, now)
    }
}

// guestLinkEnded is whether a guest's socket may no longer write or read on
// (miro-33 M-2, checked on every op): their link has run out, or the API
// turned it off or changed it after the socket was opened. Members: never.
export function guestLinkEnded(context, now = Date.now()) {
    if (context?.guest !== true) return false
    if (Number.isFinite(context.linkEndsAt) && now >= context.linkEndsAt) return true
    const closed = closedAt.get(context.user?.id)
    return closed !== undefined && !(context.connectedAt > closed)
}

// How old a guest's last authorisation may be before an op asks the server
// again (miro-33 F3): a link turned off, run out or lowered while its close
// push failed stops writing within this, whatever the close route heard.
export const GUEST_RECHECK_MS = 15_000

// How long a guest writes on while the server doesn't answer the re-check
// (miro-33 F6): from its last good answer (context.okAt, the connect's at
// first), two intervals. Then the op is refused and the socket closed for
// reauth, and the reconnect, authorised afresh, is refused while the API
// can't answer: a link turned off whose close was lost stops writing within
// this even while the API is down or out of reach.
export const GUEST_UNANSWERED_MS = 2 * GUEST_RECHECK_MS

// How much sooner than GUEST_RECHECK_MS a socket's timer may ask (V-1038):
// each wait ends a random part of this early, so guests who connected
// together (a collab restart reconnecting every tab, a class opening one
// link) spread their asks instead of all asking in the same millisecond every
// interval. Sooner, never later: both bounds above still hold.
export const GUEST_RECHECK_SPREAD_MS = 1_000

// askWithin asks the server again (context.recheck) and waits for its answer
// at most ms (V-1040): an ask out longer (the api client's 15 s timeout and
// its retries at 2, 4 and 8 s keep a hung API's ask out ~74 s) is aborted and
// answers undefined, no answer, so GUEST_UNANSWERED_MS holds when the API
// hangs, not only when it refuses fast. The client's retries inside ms go on.
async function askWithin(context, ms) {
    const abort = new AbortController()
    let timer
    try {
        return await Promise.race([
            context.recheck({ signal: abort.signal }),
            new Promise((resolve) => {
                timer = setTimeout(() => {
                    abort.abort()
                    resolve(undefined)
                }, Math.max(ms, 0))
                timer.unref?.()
            }),
        ])
    } finally {
        clearTimeout(timer)
    }
}

// unansweredTooLong is whether the server didn't answer the last ask (no
// reply, a 5xx or any other non-answer) and the last good answer is over
// GUEST_UNANSWERED_MS old.
function unansweredTooLong(context, now) {
    return context.unanswered === true && now - (context.okAt ?? 0) > GUEST_UNANSWERED_MS
}

// guestGrantGone is whether a guest's link no longer lets this socket do what
// it opened for, asking the server again (context.recheck, the guestAuthorize
// call made at connect) at most once per GUEST_RECHECK_MS per socket (less
// early, the timer's spread) and one ask at a time. Refused (401, 403, 404), or an editing socket whose link no
// longer edits: gone, and gone for good. The server not answering is no
// refusal: it is asked again after the interval, until its last good answer
// is GUEST_UNANSWERED_MS old (then the socket goes, for reauth): an ask still
// out then counts as no answer (askWithin).
// The ask reuses the connect's live token, which lasts 30 minutes
// (guestCollabTokenTTL): after that the API refuses it, so every guest socket
// (link, doc and call) is closed for reauth at its first op past 30 minutes.
// The web fetches a fresh token on every connect (lib/collabSession.ts
// token(), CallGuestBoardStage's tokenFetcher) and Yjs sends the dropped op
// again, so a guest reconnects every 30 minutes; they are not turned away.
export async function guestGrantGone(context, now = Date.now(), early = 0) {
    if (context?.guest !== true || typeof context.recheck !== 'function') return false
    if (context.gone === true) return true
    if (now - (context.checkedAt ?? 0) < GUEST_RECHECK_MS - early) return unansweredTooLong(context, now)
    if (!context.checking) {
        context.checking = (async () => {
            let res
            try {
                res = await askWithin(context, (context.okAt ?? 0) + GUEST_UNANSWERED_MS + 1 - Date.now())
            } catch (e) {
                res = e?.response
            }
            context.checkedAt = Date.now()
            const status = res?.status
            const lowered = status === 200 && context.access?.canEdit === true && res?.data?.canEdit !== true
            if (status === 401 || status === 403 || status === 404 || lowered) context.gone = true
            else if (status === 200) context.okAt = context.checkedAt
            context.unanswered = context.gone !== true && status !== 200
            return context.gone === true || unansweredTooLong(context, context.checkedAt)
        })().finally(() => {
            context.checking = null
        })
    }
    return context.checking
}

// guestStopped is the one check a guest's socket answers to, on every op
// (collabServer.js beforeHandleMessage) and on a timer while it sends nothing
// (recheckWhileOpen): its link ran out or the close route named it
// (guestLinkEnded), or the server, asked again, refuses it or hasn't answered
// for too long (guestGrantGone). Members: never.
export async function guestStopped(context, now = Date.now(), early = 0) {
    return guestLinkEnded(context, now) || (await guestGrantGone(context, now, early))
}

// recheckWhileOpen asks guestStopped about a guest's socket on a timer, so a
// guest who only watches is held to the bound one who writes is (V-1035:
// revocation covers reads too). A link turned off whose every close push was
// lost stops sending this socket the board's edits within GUEST_RECHECK_MS of
// the server refusing it, or GUEST_UNANSWERED_MS from its last good answer
// while the server doesn't answer: each wait runs to the next moment the
// check can change its answer, and an op's ask in between moves it on (one
// ask at a time, guestGrantGone). Each ask's wait ends a random part of
// GUEST_RECHECK_SPREAD_MS early (V-1038), so sockets that connected together
// don't ask together. Stopped, the socket is closed for reauth. The
// connection's close clears the timer.
export function recheckWhileOpen(connection, context) {
    if (context?.guest !== true || typeof context.recheck !== 'function') return
    let timer = null
    let open = true
    const schedule = () => {
        const early = Math.floor(Math.random() * GUEST_RECHECK_SPREAD_MS)
        let due = (context.checkedAt ?? 0) + GUEST_RECHECK_MS - early
        if (context.unanswered === true) due = Math.min(due, (context.okAt ?? 0) + GUEST_UNANSWERED_MS)
        timer = setTimeout(() => tick(early), Math.max(due - Date.now(), 0) + 1)
        timer.unref?.()
    }
    const tick = async (early) => {
        timer = null
        let stopped = false
        try {
            stopped = await guestStopped(context, Date.now(), early)
        } catch {
            // asked again at the next wait
        }
        if (!open) return
        if (stopped) closeSockets([connection.webSocket])
        else schedule()
    }
    schedule()
    connection.onClose(() => {
        open = false
        clearTimeout(timer)
    })
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
