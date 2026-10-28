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
//   - a guest who may only look (a share link's, a call's while guests can't
//     draw): nothing, {};
//   - a call's guest who draws: their pointer and button under their name,
//     nothing else (no presenter for anyone to follow);
//   - a member, reader or editor: what they publish, under their own name and
//     id (their presenting too); the colour their id gives them, and no hue of
//     their choosing (tabs work it out from the name and id).
// A tab speaks only for its own presence: what it says for a client another
// connection holds (or the server's own) is dropped.

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

// presenceAllowed is the state a person may publish, from the one their tab
// sent. null (a tab leaving) stays null.
export function presenceAllowed(state, who) {
    if (state === null) return null
    if (!isObject(state)) return {}
    if (who.guest && who.readOnly) return {}
    const named = isObject(state.user)
    if (who.guest) {
        const out = {}
        if (named) out.user = { id: who.id, name: who.name, color: colourOf(who.id) }
        const p = state.pointer
        if (isObject(p) && isNumber(p.x) && isNumber(p.y)) out.pointer = { x: p.x, y: p.y, ...(p.tool === 'pointer' || p.tool === 'laser' ? { tool: p.tool } : {}) }
        if (state.button === 'down' || state.button === 'up') out.button = state.button
        return out
    }
    const out = { ...state }
    if (named) {
        out.user = { id: who.id, name: who.name, color: colourOf(who.id) }
        if (typeof state.user.profileKey === 'string') out.user.profileKey = state.user.profileKey
    } else if ('user' in out) {
        out.user = null
    }
    if (isObject(state.presenting)) out.presenting = { ...state.presenting, userId: who.id, name: who.name }
    return out
}

// The connection holding a client's presence on this doc, if one does.
function holderOf(document, clientId) {
    for (const [socket, held] of document.connections ?? []) {
        if (held?.clients?.has(clientId)) return socket
    }
    return undefined
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
    const own = connection.webSocket
    const allowed = []
    let same = true
    for (const e of entries) {
        const holder = holderOf(document, e.clientId)
        if (e.clientId === document.awareness?.clientID || (holder !== undefined && holder !== own)) {
            same = false
            continue
        }
        const state = presenceAllowed(e.state, who)
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
