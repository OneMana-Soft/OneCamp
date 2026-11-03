// Presence and broadcasts, checked on their way in (presenceGuard.test.js).
//
// Presence (Yjs awareness) is each tab's say of who it is and where it points.
// Hocuspocus 3.4.3 applies any connection's awareness update, a read-only
// one's too, and passes it to every tab on the doc; and it passes any
// client's BroadcastStateless on to every tab as if this server had said it
// (a tab told "epoch-changed" drops its copy of the doc). Every message
// reaches beforeHandleMessage first, with the very bytes Hocuspocus reads
// next (IncomingMessage wraps a Uint8Array without copying it), so the guard
// runs there:
//   - an awareness update is applied as this server allows it, through the
//     document (so Hocuspocus still knows whose presence it is), and the
//     message's own count of states is set to 0 in place, so Hocuspocus
//     then reads no state from it;
//   - a broadcast becomes a plain stateless message to this server, which
//     takes none: it reaches no tab, and the socket stays open;
//   - a message it cannot read closes the connection, as Hocuspocus would.
//
// Who a tab is comes from its sign-in (the context onAuthenticate made): a
// member's name as the API's authorize answers it (never an address), a
// guest's from their token, as "<name> (guest)". What each may publish:
//   - every guest: who they are, under their name, and what a board's people
//     may all say (miro-24): their last few reactions and their hand, in the
//     shape tabs read them (signalsOf), nothing of their own words;
//   - a guest who may only look (a share link's, a call's while guests can't
//     draw): only that, under an id of their tab's own, since one link's
//     guests all hold the link's token and are each their own person;
//   - a call's guest who draws: their pointer and button too, nothing else
//     (no presenter for anyone to follow);
//   - a member, reader or editor: what they publish, under their own name and
//     id; the colour their id gives them, and no hue of their choosing (tabs
//     work it out from the name and id);
//   - but only an editor presents, holds everyone on their view, brings
//     everyone to it or lowers someone's hand: a member who may only read
//     publishes no presenting (a hold rides in it), no bring and no lower.
// Each member's user carries edit, set here from their sign-in (the API's
// canEdit, from the board's member list, and the connection's read-only),
// never from what their tab says: a tab honours another's presenting, hold,
// bring and lower only from a user stamped edit: true. A guest is on no
// member list: their user carries no edit, and they publish none of them.
// A tab speaks only for its own presence: what it says for a client another
// connection holds (or the server's own) is dropped, unless that connection
// is the same person's (claim): a tab that reconnects keeps its client id,
// and the server keeps the socket it left until its ping check (30-60 s
// later), so the newer connection takes the client over with its next
// change. Tabs echo the presence they hear, so an echo never moves or binds a
// client: a connection binds only its own, on a doc that has just reloaded
// too (presenceEcho.test.js). A share link names no one person, so its
// visitors never take one over.

const AWARENESS = 1
const STATELESS = 5
const BROADCAST_STATELESS = 6

// lib0's encoding, as y-protocols and Hocuspocus read it: unsigned LEB128
// numbers and length-prefixed UTF-8 strings.
class Reader {
    constructor(bytes) {
        this.bytes = bytes
        this.pos = 0
    }
    uint() {
        let n = 0
        let m = 1
        for (;;) {
            if (this.pos >= this.bytes.length) throw new Error('a message ends early')
            const b = this.bytes[this.pos++]
            n += (b & 0x7f) * m
            if (b < 0x80) return n
            m *= 128
            if (n > Number.MAX_SAFE_INTEGER) throw new Error('a number too large')
        }
    }
    span() {
        const n = this.uint()
        if (this.pos + n > this.bytes.length) throw new Error('a message ends early')
        const at = this.pos
        this.pos += n
        return this.bytes.subarray(at, at + n)
    }
    string() {
        return Buffer.from(this.span()).toString('utf8')
    }
}

const writeUint = (out, n) => {
    while (n > 0x7f) {
        out.push((n & 0x7f) | 0x80)
        n = Math.floor(n / 128)
    }
    out.push(n)
}
const writeString = (out, s) => {
    const b = Buffer.from(s, 'utf8')
    writeUint(out, b.length)
    for (const x of b) out.push(x)
}

// writeUintAt writes n over bytes[from, to) in exactly that many bytes (LEB128
// takes padded encodings: 0x85 0x00 is 5), so the message keeps its length.
function writeUintAt(bytes, from, to, n) {
    for (let i = from; i < to; i++) {
        const last = i === to - 1
        bytes[i] = (n & 0x7f) | (last ? 0 : 0x80)
        n = Math.floor(n / 128)
    }
}

// A person's colour by their id: the web app's generateColorFromUUID (its
// palette and hash), so a cursor keeps the colour its own tab gives it.
const COLOURS = ['#3B82F6', '#8B5CF6', '#EC4899', '#F59E0B', '#10B981', '#06B6D4', '#EF4444', '#6366F1', '#14B8A6', '#F97316', '#7C3AED', '#0EA5E9']
export function colourOf(id) {
    const s = id || 'default'
    let hash = 0
    for (let i = 0; i < s.length; i++) {
        hash = (hash << 5) - hash + s.charCodeAt(i)
        hash = hash & hash
    }
    return COLOURS[Math.abs(hash) % COLOURS.length]
}

// guestName is how a guest's token names them, as the call shows a guest:
// "Guest: Kai Lin" is "Kai Lin (guest)".
export function guestName(tokenName) {
    const bare = String(tokenName ?? '').replace(/^Guest:\s*/, '').trim()
    return bare && bare !== 'Guest' ? `${bare} (guest)` : 'Guest'
}

// whoIs is the person behind a connection, as its sign-in says.
export function whoIs(context, connection) {
    const guest = context?.guest === true
    const id = String(context?.user?.id ?? '')
    const name = guest ? guestName(context?.user?.name) : String(context?.user?.name || 'Anonymous')
    const readOnly = !!connection?.readOnly || context?.access?.canEdit === false
    return { id, name, guest, readOnly }
}

const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v)
const isNumber = (v) => typeof v === 'number' && Number.isFinite(v)

// A board's signals as tabs read them (the web's lib/board/signals): a tab's
// last few reactions, each { id, emoji }, newest last, and its hand,
// { id, raisedAt } while up and null once lowered. Tabs show only an emoji in
// their set; here each is kept short, and nothing else rides along.
export const REACTIONS_KEPT = 3
const isShort = (v, most) => typeof v === 'string' && v.length > 0 && v.length <= most
const isSignalId = (v) => isShort(v, 64)

// signalsOf is a guest's reactions and hand, as tabs read them: the last
// REACTIONS_KEPT well-formed reactions of a list, and a hand with an id and a
// time, or null. Anything else says nothing (no field).
export function signalsOf(state) {
    const out = {}
    if (Array.isArray(state.react)) {
        out.react = state.react
            .filter((r) => isObject(r) && isSignalId(r.id) && isShort(r.emoji, 16))
            .map((r) => ({ id: r.id, emoji: r.emoji }))
            .slice(-REACTIONS_KEPT)
    }
    const h = state.hand
    if (h === null) out.hand = null
    else if (isObject(h) && isSignalId(h.id) && isNumber(h.raisedAt)) out.hand = { id: h.id, raisedAt: h.raisedAt }
    return out
}

// presenceAllowed is the state a person may publish, from the one their tab
// sent (`clientId` is the tab's). null (a tab leaving) stays null.
export function presenceAllowed(state, who, clientId) {
    if (state === null) return null
    if (!isObject(state)) return {}
    const named = isObject(state.user)
    if (who.guest) {
        const out = {}
        if (named) {
            const id = who.readOnly ? `${who.id}:${clientId}` : who.id
            out.user = { id, name: who.name, color: colourOf(id) }
        }
        if (!who.readOnly) {
            const p = state.pointer
            if (isObject(p) && isNumber(p.x) && isNumber(p.y)) out.pointer = { x: p.x, y: p.y, ...(p.tool === 'pointer' || p.tool === 'laser' ? { tool: p.tool } : {}) }
            if (state.button === 'down' || state.button === 'up') out.button = state.button
        }
        return { ...out, ...signalsOf(state) }
    }
    const out = { ...state }
    if (named) {
        out.user = { id: who.id, name: who.name, color: colourOf(who.id), edit: !who.readOnly }
        if (typeof state.user.profileKey === 'string') out.user.profileKey = state.user.profileKey
    } else if ('user' in out) {
        out.user = null
    }
    if (who.readOnly) {
        delete out.presenting
        delete out.bring
        delete out.lower
    } else if (isObject(state.presenting)) {
        out.presenting = { ...state.presenting, userId: who.id, name: who.name }
    }
    return out
}

// personOf is the one person a sign-in names, or '' when it names no one
// person: a member by their id (the token's sub), a call's guest by their own
// sub (guest-<grant>-<suffix>; the token's call claim says it is a call's). A
// share link's visitors all sign in as the link (guest-<grant>), so a link's
// token names no one by itself: a visitor's tab names itself with a key of its
// own on every socket it opens (`tab`, the socket's tab parameter: random, 32
// hex characters, made once per tab; lib/collabSocket.ts), and the link and
// that key name the one tab, so only that tab takes its presence over (V-337).
// A tab without a well-formed key names no one. onAuthenticate keeps it on
// the connection's context (person), for claim.
const TAB_KEY = /^[0-9a-f]{32}$/
export function personOf(decoded, tab) {
    const sub = typeof decoded?.sub === 'string' ? decoded.sub : ''
    if (!sub) return ''
    if (decoded.guest !== true) return `member:${sub}`
    if (typeof decoded.call === 'string' && decoded.call) return `call-guest:${sub}`
    return typeof tab === 'string' && TAB_KEY.test(tab) ? `link-guest:${sub}:${tab}` : ''
}

const samePerson = (a, b) => typeof a?.person === 'string' && a.person !== '' && a.person === b?.person

// The connection holding a client's presence on this doc, if one does: its
// entry in document.connections ({ clients, connection }).
function holderOf(document, clientId) {
    for (const held of document.connections?.values() ?? []) {
        if (held?.clients?.has(clientId)) return held
    }
    return undefined
}

// Who last held each client on a doc (document -> client id -> person), so a
// client whose connection ended is taken again only by that person.
const lastHolders = new WeakMap()
function lastHoldersOf(document) {
    let held = lastHolders.get(document)
    if (!held) lastHolders.set(document, (held = new Map()))
    return held
}

// The one client each connection speaks for on a doc (its entry in
// document.connections -> client id): the first it binds. A tab's provider
// sends its own presence first on every connect (startSync), before it has
// heard anyone's, so what a connection says for any other client is an echo,
// or a forgery.
const ownClients = new WeakMap()

// claim answers whether a connection may speak for a client's presence (an
// entry at `clock`; `removal` when it says the client left), and makes it the
// client's holder when it may. Hocuspocus drops a client's presence when the
// connection holding it ends, and adds a client to a connection only the
// first time anyone says it. Every tab sends back the presence it hears, at
// the server's clock (@hocuspocus/provider 3.4.3 echoes every changed
// client), and when its socket closes it removes everyone else's presence and
// sends those removals once it is connected again, before or after its own.
// A doc whose every socket closed (a reauth closes them all) unloads, and the
// tabs come back to a fresh copy that knows no clock and no holder. So only an
// entry newer than the server's copy, or a client never seen, moves or binds a
// client, and only to the one connection whose own it is:
//   - a client this connection holds: yes;
//   - one another connection holds: only the same person's newer entry, and
//     the client moves here (a reconnecting tab's old socket: its end then
//     takes nothing with it, and this one's takes the client); an echo, or
//     anyone else's claim, is refused;
//   - one nobody holds: never on a removal (it is an echo: a tab's own leaving
//     is of a client it holds); otherwise only the person who last held it
//     (anyone, for a client no person held), and only a newer entry binds it
//     here; an echo passes as before and changes nothing;
//   - a connection binds one client, its own (the first it binds), so another
//     client's entry binds nothing to it, on a doc that has just reloaded too.
function claim(document, connection, context, clientId, clock, removal) {
    const holder = holderOf(document, clientId)
    const own = document.connections?.get(connection.webSocket)
    if (holder !== undefined && holder === own) return true
    const seen = document.awareness?.meta?.get(clientId)
    const newer = seen === undefined || clock > seen.clock
    if (holder !== undefined) {
        if (!newer || !samePerson(holder.connection?.context, context)) return false
    } else {
        if (removal) return false
        const last = lastHoldersOf(document).get(clientId)
        if (last !== undefined && last !== context?.person) return false
        if (!newer) return true
    }
    if (own === undefined) return holder === undefined
    const mine = ownClients.get(own)
    if (mine !== undefined && mine !== clientId) return false
    holder?.clients.delete(clientId)
    own.clients.add(clientId)
    ownClients.set(own, clientId)
    if (typeof context?.person === 'string' && context.person) lastHoldersOf(document).set(clientId, context.person)
    return true
}

// guardIncoming checks one incoming message (beforeHandleMessage's payload):
// it applies an awareness update as allowed and leaves Hocuspocus nothing of
// it, and turns a broadcast into a stateless message to this server. It
// throws on a message it cannot read (Hocuspocus closes the connection).
export function guardIncoming({ update, connection, document, context }) {
    if (!update || !connection || !document) return
    const r = new Reader(update)
    r.string() // the document's name
    const typeAt = r.pos
    const type = r.uint()
    if (type === BROADCAST_STATELESS) {
        writeUintAt(update, typeAt, r.pos, STATELESS)
        return
    }
    if (type !== AWARENESS) return

    const body = r.span()
    const a = new Reader(body)
    const entries = []
    const count = a.uint()
    const countEnd = a.pos
    for (let n = count; n > 0; n--) {
        const clientId = a.uint()
        const clock = a.uint()
        entries.push({ clientId, clock, state: JSON.parse(a.string()) })
    }

    const who = whoIs(context, connection)
    const allowed = []
    let same = true
    for (const e of entries) {
        if (e.clientId === document.awareness?.clientID || !claim(document, connection, context, e.clientId, e.clock, e.state === null)) {
            same = false
            continue
        }
        const state = presenceAllowed(e.state, who, e.clientId)
        if (JSON.stringify(state) !== JSON.stringify(e.state)) same = false
        allowed.push({ clientId: e.clientId, clock: e.clock, state })
    }
    if (same) return

    if (allowed.length) {
        const out = []
        writeUint(out, allowed.length)
        for (const e of allowed) {
            writeUint(out, e.clientId)
            writeUint(out, e.clock)
            writeString(out, JSON.stringify(e.state))
        }
        document.applyAwarenessUpdate(connection, Uint8Array.from(out))
    }
    // What Hocuspocus reads next holds no state.
    writeUintAt(body, 0, countEnd, 0)
}
