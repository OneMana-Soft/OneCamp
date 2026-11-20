// A board link's guest on a live socket (miro-33 HEAD A, M-2): their link
// is checked on every op, not only when they connect. An op after the link
// ran out (linkEndsAt) is refused, and so is one on a socket opened before
// the API turned the link off or changed it (closePeople names
// guest-<grant id>), while the guest's reconnect, authorised afresh, writes
// again. Members are never refused as guests.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { closePeople, GUEST_RECHECK_MS, GUEST_RECHECK_SPREAD_MS, GUEST_UNANSWERED_MS, guestGrantGone, guestLinkEnded, REAUTH_CLOSE } from './ops/reauth.js'
import { FakeBackend, FakeRedis, TestClient, sleep, startServer, until } from '../testkit.js'

// fakeHocuspocus holds one board with these connections, each with a socket
// that records how it was closed.
function fakeHocuspocus(...people) {
    const connections = people.map(({ id, guest, connectedAt }) => {
        const socket = { closedWith: null, close(code, reason) { this.closedWith = { code, reason } } }
        return { context: { user: { id }, guest, connectedAt }, webSocket: socket }
    })
    const doc = { getConnections: () => connections }
    return { hocuspocus: { documents: new Map([['board:b-1', doc]]) }, connections }
}

test('U1: a guest op after its link ended is refused', () => {
    const now = Date.now()
    const guest = { guest: true, user: { id: 'guest-g-ended' }, connectedAt: now - 60_000, linkEndsAt: now - 1 }
    assert.equal(guestLinkEnded(guest, now), true)
    assert.equal(guestLinkEnded({ ...guest, linkEndsAt: now }, now), true, 'the moment it ends counts as ended')
    assert.equal(guestLinkEnded({ ...guest, linkEndsAt: now + 60_000 }, now), false, 'a link still running writes')
    const { linkEndsAt, ...noEnd } = guest
    assert.equal(guestLinkEnded(noEnd, now), false, 'a link that never ends writes')
})

test('M-2: a guest op on a socket opened before its link was turned off is refused', async () => {
    const before = Date.now() - 5_000
    const { hocuspocus, connections } = fakeHocuspocus(
        { id: 'guest-g1', guest: true, connectedAt: before },
        { id: 'member-1', guest: undefined, connectedAt: before },
    )
    const { closed } = await closePeople({ body: { user_ids: ['guest-g1'] }, hocuspocus })
    assert.equal(closed, 1)
    assert.deepEqual(connections[0].webSocket.closedWith, { code: REAUTH_CLOSE.code, reason: REAUTH_CLOSE.reason })
    assert.equal(REAUTH_CLOSE.code, 4205)
    assert.equal(connections[1].webSocket.closedWith, null, "a member's socket stays open")
    assert.equal(guestLinkEnded({ guest: true, user: { id: 'guest-g1' }, connectedAt: before }), true,
        'an op racing the close on the old socket is refused')
    assert.equal(guestLinkEnded({ guest: true, user: { id: 'guest-g2' }, connectedAt: before }), false,
        "another link's guest writes on")
})

test('a guest who reconnected after a role change writes again', async () => {
    const before = Date.now() - 5_000
    const { hocuspocus } = fakeHocuspocus({ id: 'guest-g3', guest: true, connectedAt: before })
    await closePeople({ body: { user_ids: ['guest-g3'] }, hocuspocus })
    assert.equal(guestLinkEnded({ guest: true, user: { id: 'guest-g3' }, connectedAt: before }), true)
    const reconnected = { guest: true, user: { id: 'guest-g3' }, connectedAt: Date.now() + 1 }
    assert.equal(guestLinkEnded(reconnected), false, 'the reconnect was authorised afresh by the API')
})

test('a member is never refused as a guest', async () => {
    const before = Date.now() - 5_000
    const { hocuspocus } = fakeHocuspocus({ id: 'member-2', guest: undefined, connectedAt: before })
    await closePeople({ body: { user_ids: ['member-2'] }, hocuspocus })
    assert.equal(guestLinkEnded({ user: { id: 'member-2' }, connectedAt: before }), false)
    assert.equal(guestLinkEnded({ user: { id: 'member-2' }, connectedAt: before, linkEndsAt: Date.now() - 1 }), false)
    assert.equal(guestLinkEnded(undefined), false)
    assert.equal(guestLinkEnded({}), false)
})

// The op itself (collabServer.js beforeHandleMessage), not only the check it
// asks: a guest whose link ended is refused, socket closed for reauth, before
// anything reads the op.
test('U1: the server refuses a guest op after its link ended, before reading it', async () => {
    const { createCollabServer } = await import('../collabServer.js')
    const quiet = { log() {}, warn() {}, error() {} }
    const { server } = createCollabServer({ redis: {}, api: {}, backendUrl: '', getSecret: () => '', decodeToken: () => null, log: quiet })
    const op = server.hocuspocus.configuration.beforeHandleMessage
    const ended = { guest: true, user: { id: 'guest-g-op-ended' }, connectedAt: Date.now() - 60_000, linkEndsAt: Date.now() - 1 }
    await assert.rejects(op({ context: ended, documentName: 'board:b-1' }), (e) => e.code === REAUTH_CLOSE.code && e.reason === REAUTH_CLOSE.reason)
})

// F3: the close push failed (the API retries it), and nothing told this
// socket its link was turned off: its next op, once its authorisation is
// GUEST_RECHECK_MS old, asks the server again, is refused and closes.
test('F3: a guest whose close never came is refused on its next op once its check is stale', async () => {
    const { createCollabServer } = await import('../collabServer.js')
    const quiet = { log() {}, warn() {}, error() {} }
    const { server } = createCollabServer({ redis: {}, api: {}, backendUrl: '', getSecret: () => '', decodeToken: () => null, log: quiet })
    const op = server.hocuspocus.configuration.beforeHandleMessage
    let asked = 0
    const turnedOff = async () => {
        asked++
        throw Object.assign(new Error('Request failed with status code 401'), { response: { status: 401 } })
    }
    const now = Date.now()
    const missed = { guest: true, user: { id: 'guest-g-missed' }, connectedAt: now - 60_000, checkedAt: now - GUEST_RECHECK_MS - 1, access: { canEdit: true }, recheck: turnedOff }
    await assert.rejects(op({ context: missed, documentName: 'board:b-1' }), (e) => e.code === REAUTH_CLOSE.code && e.reason === REAUTH_CLOSE.reason)
    assert.equal(asked, 1, 'the server was asked once')
    await assert.rejects(op({ context: missed, documentName: 'board:b-1' }), (e) => e.code === REAUTH_CLOSE.code)
    assert.equal(asked, 1, 'a link found off stays off without asking again')
})

test('F3: a guest is asked about again at most once per interval, and only a refusal or a lowered edit ends it', async () => {
    const now = Date.now()
    const answering = (res) => {
        const ctx = { guest: true, user: { id: 'guest-g2' }, checkedAt: now - GUEST_RECHECK_MS - 1, okAt: now - GUEST_RECHECK_MS - 1, access: { canEdit: true }, asked: 0 }
        ctx.recheck = async () => {
            ctx.asked++
            if (res instanceof Error) throw res
            return res
        }
        return ctx
    }
    const fresh = { ...answering({ status: 401 }), checkedAt: now }
    assert.equal(await guestGrantGone(fresh, now), false, 'a fresh check is not asked again')
    assert.equal(fresh.asked, 0)
    const still = answering({ status: 200, data: { canEdit: true } })
    assert.equal(await guestGrantGone(still, now), false, 'a link that still edits goes on')
    assert.equal(await guestGrantGone(still, now + 1), false, 'and is not asked again at once')
    assert.equal(still.asked, 1)
    assert.equal(await guestGrantGone(answering({ status: 200, data: { canEdit: false } }), now), true, 'an editing socket whose link no longer edits')
    assert.equal(await guestGrantGone(answering(new Error('ECONNREFUSED')), now), false, 'the server not answering is no refusal')
    assert.equal(await guestGrantGone({ ...answering({ status: 401 }), guest: undefined }, now), false, 'a member is never asked as a guest')
})

// connectGuests lets n of a link's guests in at the same instant through the
// server's own connect (collabServer.js onAuthenticate), each with its own
// token, with the API stubbed: answer(nth, id) is its nth answer to guest
// id's guestAuthorize (a number is a status; an Error is no answer at all; a
// promise of either an answer that comes later, or never). Each guest's asks
// are recorded with their time and the signal they were sent with. The clock
// is faked (apis: Date, and setTimeout for a test of the server's own
// timers), so an op can come any time after the connect.
async function connectGuests(t, n, answer, apis = ['Date']) {
    t.mock.timers.enable({ apis, now: 1_763_000_000_000 })
    const { createCollabServer } = await import('../collabServer.js')
    const quiet = { log() {}, warn() {}, error() {} }
    const asks = new Map()
    const api = {
        async post(url, body, opts) {
            const id = `guest-${String(opts?.headers?.Authorization ?? '').replace('Bearer ', '')}`
            const list = asks.get(id) ?? []
            list.push({ at: Date.now(), signal: opts?.signal })
            asks.set(id, list)
            const a = await answer(list.length, id)
            if (a instanceof Error) throw a
            if (a === 200) return { status: 200, data: { canEdit: true } }
            throw Object.assign(new Error(`Request failed with status code ${a}`), { response: { status: a } })
        },
    }
    const decodeToken = (token) => ({ guest: true, sub: `guest-${token}`, name: `Guest: ${token}` })
    const { server } = createCollabServer({ redis: {}, api, backendUrl: 'http://api', getSecret: () => '', decodeToken, log: quiet })
    const { onAuthenticate, beforeHandleMessage, connected } = server.hocuspocus.configuration
    const guests = []
    for (let i = 0; i < n; i++) {
        const id = `guest-live-${i}`
        const connectionConfig = {}
        const context = await onAuthenticate({ token: `live-${i}`, documentName: 'board:b-1', connectionConfig })
        assert.equal(asks.get(id)?.length, 1, 'asked once at connect')
        assert.notEqual(connectionConfig.readOnly, true, 'an edit link writes')
        // listen hands the server's own connected hook a tab that sends
        // nothing: its socket records how it was closed, and a close (the
        // server's, or disconnect: the tab going away) ends the connection,
        // as Hocuspocus's.
        const listen = async () => {
            const closers = []
            const gone = () => { for (const c of closers.splice(0)) c() }
            const socket = { closedWith: null, close(code, reason) { this.closedWith ??= { code, reason }; gone() } }
            await connected({ context, connection: { webSocket: socket, onClose: (c) => closers.push(c) }, documentName: 'board:b-1' })
            return { socket, disconnect: gone }
        }
        const own = () => asks.get(id) ?? []
        guests.push({ id, op: () => beforeHandleMessage({ context, documentName: 'board:b-1' }), asked: () => own().length, asks: own, listen })
    }
    return guests
}

// connectGuest is connectGuests with one guest.
async function connectGuest(t, answer, apis = ['Date']) {
    const [guest] = await connectGuests(t, 1, answer, apis)
    return guest
}

const reauth = (e) => e.code === REAUTH_CLOSE.code && e.reason === REAUTH_CLOSE.reason

// F4: the per-op re-check at its seam. The connect itself hands the op the
// call to ask again with: a guest let in at connect, whose link the API
// refuses afterwards, is refused on its first op once its check is stale.
test('F4: a guest let in at connect is refused by the server once its link is refused, through the context the connect built', async (t) => {
    const { op, asked } = await connectGuest(t, (n) => (n === 1 ? 200 : 401))
    await op()
    assert.equal(asked(), 1, 'a fresh check is not asked again')
    t.mock.timers.tick(GUEST_RECHECK_MS + 1)
    await assert.rejects(op(), reauth, 'the op after the link was refused closes the socket for reauth')
    assert.equal(asked(), 2, 'the server was asked again with the connect\'s own call')
})

// F6: the server not answering lets a guest write on only for a while. Once
// its last good answer is 2 x GUEST_RECHECK_MS old and the server still has
// not answered, the op is refused and the socket closed for reauth (the
// reconnect is authorised afresh, and refused while the API can't answer).
for (const [what, failure] of [
    ['a 503', () => 503],
    ['no answer', () => Object.assign(new Error('connect ECONNREFUSED'), { code: 'ECONNREFUSED' })],
]) {
    test(`F6: a guest whose server gives ${what} writes on for 2 x GUEST_RECHECK_MS from its last good answer, and not after`, async (t) => {
        const { op, asked } = await connectGuest(t, (n) => (n === 1 ? 200 : failure()))
        t.mock.timers.tick(20_000)
        await op()
        assert.equal(asked(), 2, 'asked again at 20 s, and not answered')
        t.mock.timers.tick(2 * GUEST_RECHECK_MS - 20_000 + 1)
        await assert.rejects(op(), reauth, 'an op past 2 x GUEST_RECHECK_MS with no good answer is refused')
    })
}

test('F6: a good answer starts the while again', async (t) => {
    const { op, asked } = await connectGuest(t, (n) => (n <= 2 ? 200 : 503))
    t.mock.timers.tick(GUEST_RECHECK_MS + 1)
    await op()
    t.mock.timers.tick(2 * GUEST_RECHECK_MS - 1)
    await op()
    assert.equal(asked(), 3, 'asked at connect, at 15 s (answered) and at 45 s (not answered)')
})

// V-1035 (MAIN 19:4x): revocation covers reads too. A guest's socket that
// sends nothing is asked about on a timer, by the same check an op asks
// (guestLinkEnded, then guestGrantGone), within the same bound: refused,
// it is closed within GUEST_RECHECK_MS; with no answer, GUEST_UNANSWERED_MS
// from its last good one. No close push from the API is needed (every close
// try may have failed), and a connection that ends takes its timer with it.
const timers = ['Date', 'setTimeout']
const settle = () => new Promise((resolve) => setImmediate(resolve))
const noAnswer = () => Object.assign(new Error('connect ECONNREFUSED'), { code: 'ECONNREFUSED' })

test('V-1035: a guest who sends nothing is closed within GUEST_RECHECK_MS once the server refuses its link, with no close from the API', async (t) => {
    const { listen, asked } = await connectGuest(t, (n) => (n === 1 ? 200 : 401), timers)
    const { socket } = await listen()
    t.mock.timers.tick(GUEST_RECHECK_MS - GUEST_RECHECK_SPREAD_MS)
    await settle()
    assert.equal(asked(), 1, 'not asked again before the interval, less its spread')
    assert.equal(socket.closedWith, null, 'open while its check is fresh')
    t.mock.timers.tick(GUEST_RECHECK_SPREAD_MS + 1)
    await settle()
    assert.equal(asked(), 2, 'asked again by the interval, with no op')
    assert.deepEqual(socket.closedWith, REAUTH_CLOSE, 'the refused guest is closed for reauth')
})

test('V-1035: a guest who sends nothing, whose server never answers, is closed GUEST_UNANSWERED_MS from its last good answer, and not before', async (t) => {
    const { listen, asked } = await connectGuest(t, (n) => (n === 1 ? 200 : noAnswer()), timers)
    const { socket } = await listen()
    t.mock.timers.tick(GUEST_RECHECK_MS + 1)
    await settle()
    assert.equal(asked(), 2, 'asked again at the interval')
    assert.equal(socket.closedWith, null, 'one ask not answered is no refusal')
    t.mock.timers.tick(GUEST_UNANSWERED_MS - GUEST_RECHECK_MS - 1)
    await settle()
    assert.equal(socket.closedWith, null, 'open until GUEST_UNANSWERED_MS from its last good answer')
    t.mock.timers.tick(1)
    await settle()
    assert.deepEqual(socket.closedWith, REAUTH_CLOSE, 'past it, closed for reauth')
})

test('V-1035: a guest still let in is asked each interval, and a connection that ends is never asked about again', async (t) => {
    const { listen, asked } = await connectGuest(t, () => 200, timers)
    const { socket, disconnect } = await listen()
    t.mock.timers.tick(GUEST_RECHECK_MS + 1)
    await settle()
    assert.equal(asked(), 2, 'asked at the interval')
    t.mock.timers.tick(GUEST_RECHECK_MS + 1)
    await settle()
    assert.equal(asked(), 3, 'and at the next')
    disconnect()
    t.mock.timers.tick(10 * GUEST_RECHECK_MS)
    await settle()
    assert.equal(asked(), 3, 'its timer went with the connection')
    assert.equal(socket.closedWith, null, 'a guest still let in is never closed')
})

// V-1040: the "no answer" bound holds when the API hangs, not only when it
// refuses fast. One ask through the api client (a 15 s timeout, then retries
// at 2, 4 and 8 s) can stay out ~74 s; an ask that has not settled
// GUEST_UNANSWERED_MS from the last good answer is given up on (aborted, and
// counted as no answer), so the socket is closed for reauth and an op waiting
// on the ask is refused. An answer that comes late but inside the bound, the
// client retrying, is taken.
const never = () => new Promise(() => {})
const outcome = (op) => op.then(() => 'let in', (e) => (reauth(e) ? 'refused' : e))
const settled = (op) => Promise.race([op, settle().then(() => 'still waiting')])

test('V-1040: a guest whose server hangs is closed GUEST_UNANSWERED_MS from its last good answer, and an op waiting on the ask is refused', async (t) => {
    const { listen, op, asks } = await connectGuest(t, (n) => (n === 1 ? 200 : never()), timers)
    const { socket } = await listen()
    t.mock.timers.tick(GUEST_RECHECK_MS + 1)
    await settle()
    assert.equal(asks().length, 2, "the timer's ask, which never settles")
    t.mock.timers.tick(5_000)
    const late = outcome(op())
    t.mock.timers.tick(GUEST_UNANSWERED_MS - GUEST_RECHECK_MS - 5_000 - 1)
    await settle()
    assert.equal(socket.closedWith, null, 'open until GUEST_UNANSWERED_MS from its last good answer')
    t.mock.timers.tick(1)
    await settle()
    assert.deepEqual(socket.closedWith, REAUTH_CLOSE, 'past it, closed for reauth, though the ask never settled')
    assert.equal(await settled(late), 'refused', 'the op waiting on the ask is refused')
    assert.equal(asks()[1].signal?.aborted, true, 'the ask given up on is aborted')
    assert.equal(asks().length, 2, 'one ask at a time')
})

test('V-1040: the bound runs from the last good answer, not from the hung ask', async (t) => {
    const { listen, asks } = await connectGuest(t, (n) => (n <= 2 ? 200 : never()), timers)
    const { socket } = await listen()
    t.mock.timers.tick(GUEST_RECHECK_MS + 1)
    await settle()
    const okAt = asks()[1].at // answered at once: its last good answer
    t.mock.timers.tick(GUEST_RECHECK_MS + 1)
    await settle()
    assert.equal(asks().length, 3, "the timer's next ask, which never settles")
    t.mock.timers.tick(okAt + GUEST_UNANSWERED_MS - Date.now())
    await settle()
    assert.equal(socket.closedWith, null, 'open until GUEST_UNANSWERED_MS from the last good answer')
    t.mock.timers.tick(1)
    await settle()
    assert.deepEqual(socket.closedWith, REAUTH_CLOSE, 'past it, closed for reauth')
})

// The bound runs from the last GOOD answer even when the ask before the hung
// one came back at once as no answer (a 400 the client returns at once, a 503
// after its retries): then the last ask is later than the last good answer.
// The timer's spread is pinned to its most, so each wait ends ~1 s early and
// the hung ask goes out before the bound.
for (const status of [400, 503]) {
    test(`V-1040: a hang after a ${status} (no answer) still closes GUEST_UNANSWERED_MS from the last good answer`, async (t) => {
        const { listen, asks } = await connectGuest(t, (n) => (n <= 2 ? 200 : n === 3 ? status : never()), timers)
        t.mock.method(Math, 'random', () => 0.999)
        const { socket } = await listen()
        const wait = GUEST_RECHECK_MS - GUEST_RECHECK_SPREAD_MS + 2 // each timer's wait, from its last answer
        t.mock.timers.tick(wait)
        await settle()
        const okAt = asks()[1].at // answered 200: the last good answer
        t.mock.timers.tick(wait)
        await settle()
        assert.equal(asks().length, 3, `the next ask, answered ${status}: no answer`)
        t.mock.timers.tick(wait)
        await settle()
        assert.equal(asks().length, 4, 'the next ask, which never settles')
        t.mock.timers.tick(okAt + GUEST_UNANSWERED_MS - 1 - Date.now())
        await settle()
        assert.equal(socket.closedWith, null, 'open at the last good answer + GUEST_UNANSWERED_MS - 1')
        t.mock.timers.tick(2)
        await settle()
        assert.deepEqual(socket.closedWith, REAUTH_CLOSE, 'closed for reauth past GUEST_UNANSWERED_MS from the last good answer, not from the last ask')
        assert.equal(asks()[3].signal?.aborted, true, 'the hung ask is aborted')
    })
}

test('V-1040: an answer that comes late but inside the bound (the api client retrying) is taken, and its ask not aborted', async (t) => {
    const slow = () => new Promise((resolve) => setTimeout(() => resolve(200), GUEST_RECHECK_MS - 3_000))
    const { listen, op, asks } = await connectGuest(t, (n) => (n === 2 ? slow() : 200), timers)
    const { socket } = await listen()
    t.mock.timers.tick(GUEST_RECHECK_MS + 1)
    await settle()
    assert.equal(asks().length, 2, "the timer's ask, answered later")
    t.mock.timers.tick(GUEST_RECHECK_MS - 3_000)
    await settle()
    t.mock.timers.tick(GUEST_UNANSWERED_MS - GUEST_RECHECK_MS + 3_000)
    await settle()
    assert.equal(socket.closedWith, null, 'its late good answer starts the while again')
    assert.notEqual(asks()[1].signal?.aborted, true, 'an ask answered inside the bound is not aborted')
    assert.equal(await settled(outcome(op())), 'let in', 'the guest writes on')
})

// V-1038: guests who connect together (a collab restart reconnecting every
// tab, a class opening one link) don't all ask in the same millisecond every
// interval: each socket's wait ends a random part of GUEST_RECHECK_SPREAD_MS
// early, and never late, so each ask still comes within GUEST_RECHECK_MS of
// its last answer.
test('V-1038: 20 guests connected in one millisecond ask at spread times each interval, each within GUEST_RECHECK_MS of its last answer', async (t) => {
    const N = 20
    const guests = await connectGuests(t, N, () => 200, timers)
    for (const g of guests) await g.listen()
    // A millisecond at a time, so each ask's time is the moment it was made.
    for (let ms = 0; ms < 2 * GUEST_RECHECK_MS + 2; ms++) {
        t.mock.timers.tick(1)
        await settle()
    }
    for (const round of [1, 2]) {
        const times = new Set(guests.map((g) => g.asks()[round]?.at))
        assert.ok(times.size >= N / 2, `round ${round}: ${N} guests asked at ${times.size} distinct times`)
    }
    for (const g of guests) {
        const at = g.asks().map((a) => a.at)
        assert.equal(at.length, 3, `${g.id} asked at connect and twice since`)
        for (let k = 1; k < at.length; k++) {
            const gap = at[k] - at[k - 1]
            assert.ok(gap >= GUEST_RECHECK_MS - GUEST_RECHECK_SPREAD_MS && gap <= GUEST_RECHECK_MS + 1, `${g.id} asked ${gap} ms after its last answer`)
        }
    }
})

// The timer's cost and its leaks, pinned (CODE-REVIEWER-314's R314-C, D and
// E, CODE-REVIEWER-B94's B94-L1): asked once per interval, never again inside
// a fresh answer, and never after its connection closes.
test('R314-C: of N guests, half sending an op every second, half silent, each is asked once per GUEST_RECHECK_MS; an active one no more than its ops alone would', async (t) => {
    const N = 40
    const SECONDS = 120
    const guests = await connectGuests(t, N, () => 200, timers)
    const sockets = []
    for (const g of guests) sockets.push((await g.listen()).socket)
    const active = guests.slice(0, N / 2)
    for (let s = 0; s < SECONDS; s++) {
        await Promise.all(active.map((g) => g.op()))
        t.mock.timers.tick(1000)
        await settle()
    }
    const rounds = Math.floor((SECONDS * 1000) / GUEST_RECHECK_MS)
    for (const g of active) assert.ok(g.asked() - 1 <= rounds, `an active guest asked ${g.asked() - 1} times in ${SECONDS} s, at most ${rounds}`)
    for (const g of guests.slice(N / 2)) {
        const n = g.asked() - 1
        assert.ok(n >= rounds - 1 && n <= rounds, `a silent guest asked ${n} times in ${SECONDS} s, want ${rounds - 1}..${rounds}`)
    }
    for (const socket of sockets) assert.equal(socket.closedWith, null)
})

test('R314-D: a fresh answer from an op inside the window is not asked again by the timer', async (t) => {
    const { listen, op, asked } = await connectGuest(t, () => 200, timers)
    t.mock.method(Math, 'random', () => 0) // no spread: the timer is due 1 ms after the op
    await listen()
    t.mock.timers.tick(GUEST_RECHECK_MS)
    await op()
    await settle()
    assert.equal(asked(), 2, "the op's ask")
    t.mock.timers.tick(1)
    await settle()
    t.mock.timers.tick(GUEST_RECHECK_MS - 2)
    await settle()
    assert.equal(asked(), 2, 'the timer, waking inside the fresh window, asks nothing')
})

test('R314-E: closing N sockets, one mid-ask, stops every timer; nothing is asked after', async (t) => {
    let hold = null
    const guests = await connectGuests(t, 10, (nth, id) => (id === 'guest-live-0' && nth === 2 ? new Promise((resolve) => { hold = () => resolve(200) }) : 200), timers)
    const tabs = []
    for (const g of guests) tabs.push(await g.listen())
    t.mock.timers.tick(GUEST_RECHECK_MS + 1)
    await settle()
    assert.ok(hold, 'guest 0 is mid-ask')
    for (const tab of tabs) tab.disconnect()
    hold()
    await settle()
    const before = guests.map((g) => g.asked())
    t.mock.timers.tick(20 * GUEST_RECHECK_MS)
    await settle()
    assert.deepEqual(guests.map((g) => g.asked()), before, 'no socket is asked about after its close')
    for (const tab of tabs) assert.equal(tab.socket.closedWith, null, 'nothing closes a socket already gone')
})

for (const status of [200, 401]) {
    test(`B94-L1 (${status}): a close during the timer's ask leaves no timer behind`, async (t) => {
        let answer
        const { listen, asked } = await connectGuest(t, (n) => (n === 2 ? new Promise((resolve) => { answer = resolve }) : 200), timers)
        const { socket, disconnect } = await listen()
        t.mock.timers.tick(GUEST_RECHECK_MS + 1)
        await settle()
        assert.equal(asked(), 2, 'the timer asked once, at the interval')
        disconnect() // the tab goes away mid-ask
        answer(status)
        await settle()
        for (let i = 0; i < 10; i++) {
            t.mock.timers.tick(GUEST_RECHECK_MS + 1)
            await settle()
        }
        assert.equal(asked(), 2, 'nothing asked after the close')
        assert.equal(socket.closedWith, null, 'a closed connection is not closed again')
    })
}

// The same, end to end on a real server with real sockets and real time: a
// share link's guest who only watches hears the board's edits, its link is
// turned off and no close ever reaches the service, and within
// GUEST_RECHECK_MS its socket is closed and the board's later edits never
// reach it. The member's socket stays.
test("V-1035: a guest who only watches stops hearing the board's edits within GUEST_RECHECK_MS of its link being turned off, with no close from the API", async () => {
    const id = '5d0c6a1e-2b3f-4c5d-8e9f-0a1b2c3d4e5f'
    const name = `board:${id}`
    const backend = new FakeBackend()
    backend.boards.set(id, { board_state: '' })
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const member = new TestClient({ url: collab.url, name, token: '{"sub":"user-1"}' })
    const guest = new TestClient({ url: collab.url, name, token: '{"guest":true,"sub":"guest-g-silent","name":"Guest: Sid"}' })
    assert.equal(await member.connect(), 'synced')
    assert.equal(await guest.connect(), 'synced')
    let closed = null
    guest.ws.once('close', (code, reason) => { closed = { code, reason: String(reason) } })
    const draw = (eid) => member.doc.getMap('elements').set(eid, { id: eid, type: 'rectangle', x: 0, y: 0, version: 1, versionNonce: 1, isDeleted: false })
    const heard = (eid) => guest.doc.getMap('elements').has(eid)
    draw('before')
    await until(() => heard('before'), 3000, 'the guest hearing an edit while its link works')
    backend.levels.set('guest-g-silent', 'none') // turned off; its close push never comes
    const off = Date.now()
    await until(() => closed !== null, GUEST_RECHECK_MS + 5000, "the silent guest's socket closing")
    assert.ok(Date.now() - off <= GUEST_RECHECK_MS + 2000, `closed ${Date.now() - off} ms after its link was turned off`)
    assert.deepEqual(closed, REAUTH_CLOSE, 'closed for reauth')
    draw('after')
    await until(() => collab.server.hocuspocus.documents.get(name)?.getMap('elements').has('after'), 3000, "the member's edit reaching the board")
    await sleep(200)
    assert.equal(heard('after'), false, 'an edit after the close never reaches the guest')
    assert.equal(member.ws.readyState, 1, "the member's socket stays open")
})
