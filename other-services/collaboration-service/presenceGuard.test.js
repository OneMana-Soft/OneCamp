// Presence and broadcasts, checked on their way in.
//
// Presence (awareness) is every tab's say of who it is and where it points;
// Hocuspocus passes any connection's on to every tab on the doc, read-only or
// not, and passes any client's broadcast on as if the server had said it. So a
// tab could put any name on its cursor, steer whoever follows a presenter, or
// tell every tab to drop its copy of the doc. Now the server decides:
//   - who a tab is comes from its sign-in: a member's name as the API answers
//     it (never an address), a guest's from their token, as "<name> (guest)";
//   - a guest who may only look shows nothing of where they are, only who
//     they are, under a name of their own (one link's guests are each their
//     own person), and what every guest may say (miro-24 W3-B2): their last
//     few reactions and their hand, in the shape tabs read them;
//   - a call's guest who draws shows their pointer too, nothing else (no
//     presenter to follow);
//   - a member who may only read keeps their presence (a doc's "viewing" list),
//     but only an editor presents, holds everyone, brings everyone or lowers
//     a hand: a reader's presenting, bring and lower reach nobody, and a
//     guest's are never passed on;
//   - each member's user says whether they may edit (edit), as the server
//     knows it, never as their tab claims; a guest's says nothing;
//   - a tab speaks only for its own presence, and a tab that reconnects
//     speaks for it at once: a member's or a call guest's as who they are, a
//     share link visitor's by its tab's own key (V-337); a socket the tab has
//     left takes nothing with it when it ends (V-353), and a socket carrying
//     several docs speaks for each doc's own client (V-348);
//   - a client's broadcast reaches no other tab, and its socket stays open.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { randomBytes } from 'node:crypto'
import { FakeBackend, FakeRedis, TestClient, ended, startServer, until, sleep } from './testkit.js'
import { personOf } from './presenceGuard.js'

const BOARD_ID = '5b0f7a52-3c1e-4d8a-9f6b-2a7c4e1d9b30'
const BOARD = `board:${BOARD_ID}`
const DOC = 'c41f0a9e-6b2d-4e7a-8c15-3f9d2b7e6a10'
const VIEW = { scrollX: 0, scrollY: 0, zoom: 1, width: 800, height: 600 }
const COLOUR = /^#[0-9A-F]{6}$/

const SAM = '{"sub":"u-sam"}'
const MAYA = '{"sub":"u-maya"}'
// A share link's guest (the API answers read-only) and a call's guest, whose
// token names them as the call does.
const LINK_GUEST = '{"guest":true,"sub":"g-link","name":"Guest"}'
const CALL_GUEST = '{"guest":true,"sub":"guest-kai","name":"Guest: Kai Lin","call":"s-1"}'

async function room(name = BOARD) {
    const backend = new FakeBackend()
    backend.boards.set(BOARD_ID, { board_state: '' })
    backend.doc(DOC, '<p>hello</p>')
    backend.names.set('u-sam', 'Sam Rivera')
    backend.names.set('u-maya', 'Maya Chen')
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const open = async (token, level) => {
        if (level) backend.levels.set(JSON.parse(token).sub, level)
        const tab = new TestClient({ url: collab.url, name, token })
        assert.equal(await tab.connect(), 'synced')
        return tab
    }
    return { backend, collab, open }
}

// A tab's old socket ends after the tab has taken its presence over on a new
// one (V-353). The relay never closes a superseded socket: the transport ends
// it, or the ping check 30-60 s later. Its end takes nothing the new socket
// holds: the server keeps the tab's presence, nobody hears the tab leave, and
// the new socket goes on speaking for it. Each move is a doc's new connection
// (again), who hears the tab there, and how (heard, said); `riding` are the
// old socket's other docs.
async function oldSocketEnds(collab, { old, riding = [], moves }) {
    const left = moves.map(({ hears }) => hears.removals.length)
    await ended(collab, old, ...riding)
    for (const [i, { again, hears, heard, said }] of moves.entries()) {
        const name = again.name.slice(0, 5)
        const id = again.doc.clientID
        const held = collab.server.hocuspocus.documents.get(again.name).awareness.getStates().has(id)
        assert.ok(held, `${name}: the server keeps the tab's presence when its old socket ends`)
        again.presence(said(9))
        await until(() => heard(9), 1000, `${name}: the new socket speaks for the tab within 1 s of the old one's end`)
        assert.ok(!hears.removals.slice(left[i]).includes(id), `${name}: nobody hears the tab leave when its old socket ends`)
    }
}

test('a guest who may only look shows only who they are, under a name of their own, whatever else it sends', async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const guest = await open(LINK_GUEST)
    assert.equal(guest.scope, 'readonly')
    guest.presence({ pointer: { x: 1, y: 2 }, button: 'down', presenting: { userId: 'u-sam', name: 'Sam Rivera', view: VIEW } })
    await until(() => sam.others.has(guest.doc.clientID), 3000, "Sam hears the guest's client")
    assert.deepEqual(sam.others.get(guest.doc.clientID), {})

    // Saying who they are, the tab claims Sam's name, an editor's stamp, his view and a follow: only the name is the
    // server's, the link's own for this tab.
    guest.presence({ user: { id: 'u-sam', name: 'Sam Rivera', edit: true, profileKey: 'k-sam' }, pointer: { x: 1, y: 2 }, button: 'down', bring: { id: 'b-g', view: VIEW }, following: sam.doc.clientID, view: VIEW })
    await until(() => sam.others.get(guest.doc.clientID)?.user, 3000, 'Sam hears who the guest is')
    const g = sam.others.get(guest.doc.clientID)
    assert.deepEqual(Object.keys(g), ['user'])
    assert.deepEqual(Object.keys(g.user).sort(), ['color', 'id', 'name'])
    assert.deepEqual({ id: g.user.id, name: g.user.name }, { id: `g-link:${guest.doc.clientID}`, name: 'Guest' })
    assert.match(g.user.color, COLOUR)
})

test("a guest reacts and raises a hand under their own name, a link's or a call's; their forged lower reaches nobody", async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const ann = await open(LINK_GUEST)
    const bo = await open(LINK_GUEST)
    const kai = await open(CALL_GUEST, 'edit')
    const heart = [{ id: 'r-ann', emoji: '❤️' }]
    // Ann's tab claims an editor's stamp, and a lower for Sam's tab.
    ann.presence({ user: { id: 'u-sam', name: 'Sam Rivera', edit: true }, react: heart, hand: { id: 'h-ann', raisedAt: 1000 }, lower: [{ id: 'l-ann', client: sam.doc.clientID }] })
    bo.presence({ user: {}, hand: { id: 'h-bo', raisedAt: 2000 } })
    kai.presence({ user: { id: 'guest-kai' }, pointer: { x: 5, y: 6 }, button: 'up', react: [{ id: 'r-kai', emoji: '👍' }], hand: { id: 'h-kai', raisedAt: 3000 }, lower: [{ id: 'l-kai', client: sam.doc.clientID }] })
    await until(() => [ann, bo, kai].every((t) => sam.others.get(t.doc.clientID)?.user), 3000, 'Sam hears all three guests')

    const a = sam.others.get(ann.doc.clientID)
    assert.deepEqual(Object.keys(a).sort(), ['hand', 'react', 'user'])
    assert.deepEqual(Object.keys(a.user).sort(), ['color', 'id', 'name'])
    assert.deepEqual({ name: a.user.name, react: a.react, hand: a.hand }, { name: 'Guest', react: heart, hand: { id: 'h-ann', raisedAt: 1000 } })
    // Two guests on one link are two people, so each hand has its own place in the queue.
    const b = sam.others.get(bo.doc.clientID)
    assert.notEqual(a.user.id, b.user.id)
    assert.deepEqual(b.hand, { id: 'h-bo', raisedAt: 2000 })
    const k = sam.others.get(kai.doc.clientID)
    assert.deepEqual(Object.keys(k).sort(), ['button', 'hand', 'pointer', 'react', 'user'])
    assert.deepEqual({ id: k.user.id, name: k.user.name, react: k.react, hand: k.hand }, { id: 'guest-kai', name: 'Kai Lin (guest)', react: [{ id: 'r-kai', emoji: '👍' }], hand: { id: 'h-kai', raisedAt: 3000 } })

    // A hand comes down as null.
    ann.presence({ user: {}, react: heart, hand: null })
    await until(() => sam.others.get(ann.doc.clientID)?.hand === null, 3000, "Ann's hand comes down")
})

test("a guest's reactions and hand pass only as tabs read them: their last few well-formed reactions, a hand with an id and a time", async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const ann = await open(LINK_GUEST)
    const react = [
        { id: 'r1', emoji: '👍' },
        { id: 'x'.repeat(65), emoji: '👍' },
        { id: 'r2', emoji: 'x'.repeat(17) },
        'r3',
        { id: 'r4', emoji: '😮' },
        { id: 'r5', emoji: '👏', note: 'a word of their own' },
        { id: 'r6', emoji: '😂' },
    ]
    ann.presence({ user: {}, react, hand: { id: 'h1', raisedAt: 'soon' } })
    await until(() => sam.others.get(ann.doc.clientID)?.user, 3000, 'Sam hears the guest')
    const a = sam.others.get(ann.doc.clientID)
    assert.deepEqual(a.react, [{ id: 'r4', emoji: '😮' }, { id: 'r5', emoji: '👏' }, { id: 'r6', emoji: '😂' }])
    assert.equal('hand' in a, false)

    // Reactions that aren't a list, and a hand with no id, say nothing.
    ann.presence({ user: {}, react: { id: 'r7', emoji: '👍' }, hand: { raisedAt: 5 } })
    await until(() => !('react' in (sam.others.get(ann.doc.clientID) ?? { react: [] })), 3000, "the guest's reactions go")
    assert.deepEqual(Object.keys(sam.others.get(ann.doc.clientID)), ['user'])
})

test("nobody wears another's name: a member is named by the API, a drawing guest by their token", async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const maya = await open(MAYA)
    const kai = await open(CALL_GUEST, 'edit')
    assert.equal(kai.scope, 'read-write')
    maya.presence({ user: { id: 'u-sam', name: 'Sam Rivera', color: '#000000', hue: 'red', profileKey: 'k-maya' }, pointer: { x: 3, y: 4 }, button: 'up', mode: 'editing' })
    kai.presence({ user: { id: 'u-sam', name: 'Sam Rivera', profileKey: 'k-sam', edit: true }, pointer: { x: 5, y: 6 }, button: 'down', mode: 'editing', bring: { id: 'b-kai', view: VIEW } })
    await until(() => sam.others.get(maya.doc.clientID)?.user && sam.others.get(kai.doc.clientID)?.user, 3000, 'Sam hears both')

    const m = sam.others.get(maya.doc.clientID)
    assert.deepEqual({ id: m.user.id, name: m.user.name, profileKey: m.user.profileKey, hue: m.user.hue }, { id: 'u-maya', name: 'Maya Chen', profileKey: 'k-maya', hue: undefined })
    assert.match(m.user.color, COLOUR)
    assert.notEqual(m.user.color, '#000000')
    assert.deepEqual({ pointer: m.pointer, button: m.button, mode: m.mode }, { pointer: { x: 3, y: 4 }, button: 'up', mode: 'editing' })

    // A drawing guest: their pointer under the call's name for them, and nothing else.
    const k = sam.others.get(kai.doc.clientID)
    assert.deepEqual(Object.keys(k).sort(), ['button', 'pointer', 'user'])
    assert.deepEqual(Object.keys(k.user).sort(), ['color', 'id', 'name'])
    assert.deepEqual({ id: k.user.id, name: k.user.name }, { id: 'guest-kai', name: 'Kai Lin (guest)' })
    assert.deepEqual({ pointer: k.pointer, button: k.button }, { pointer: { x: 5, y: 6 }, button: 'down' })
})

test("a guest's presenting leaves nobody a presenter to follow; a member's says who presents", async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const maya = await open(MAYA)
    const kai = await open(CALL_GUEST, 'edit')
    kai.presence({ user: { name: 'Kai Lin (guest)' }, pointer: { x: 1, y: 1 }, presenting: { userId: 'u-sam', name: 'Sam Rivera', view: VIEW } })
    maya.presence({ user: { id: 'u-maya', name: 'Maya Chen' }, presenting: { userId: 'u-sam', name: 'Sam Rivera', view: VIEW } })
    sam.presence({ user: { id: 'u-sam', name: 'Sam Rivera' }, pointer: { x: 7, y: 8 }, button: 'up', presenting: { userId: 'u-sam', name: 'Sam Rivera', view: VIEW } })
    await until(() => maya.others.get(kai.doc.clientID)?.user && maya.others.get(sam.doc.clientID)?.user && sam.others.get(maya.doc.clientID)?.user, 3000, 'everyone heard')

    // A board follows any state with presenting.view (boardFacilitation): a guest's has none.
    assert.equal(maya.others.get(kai.doc.clientID).presenting, undefined)
    // A member presents under their own name, whatever their tab says.
    assert.deepEqual(sam.others.get(maya.doc.clientID).presenting, { userId: 'u-maya', name: 'Maya Chen', view: VIEW })
    // And a member's cursor and presenting reach the others as they were.
    const s = maya.others.get(sam.doc.clientID)
    assert.equal(s.user.name, 'Sam Rivera')
    assert.deepEqual({ pointer: s.pointer, button: s.button, presenting: s.presenting }, { pointer: { x: 7, y: 8 }, button: 'up', presenting: { userId: 'u-sam', name: 'Sam Rivera', view: VIEW } })
})

test('a member who may only read keeps their presence, under their own name, on a board and on a doc', async () => {
    for (const name of [BOARD, DOC]) {
        const { open } = await room(name)
        const sam = await open(SAM)
        const maya = await open(MAYA, 'view')
        assert.equal(maya.scope, 'readonly', name)
        maya.presence({ user: { id: 'u-sam', name: 'Sam Rivera' }, pointer: { x: 9, y: 9 }, mode: 'viewing' })
        await until(() => sam.others.get(maya.doc.clientID)?.user, 3000, `Sam hears Maya on ${name}`)
        const m = sam.others.get(maya.doc.clientID)
        assert.deepEqual({ id: m.user.id, name: m.user.name, mode: m.mode, pointer: m.pointer, edit: m.user.edit }, { id: 'u-maya', name: 'Maya Chen', mode: 'viewing', pointer: { x: 9, y: 9 }, edit: false }, name)
    }
})

test("only an editor presents, holds or brings everyone: a reader's reach nobody, and each member's edit is the server's, whatever their tab claims", async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const maya = await open(MAYA, 'view')
    assert.equal(maya.scope, 'readonly')
    // Maya may only read; her tab claims an editor's user, presenting, hold and bring.
    maya.presence({ user: { id: 'u-maya', name: 'Maya Chen', edit: true }, pointer: { x: 1, y: 2 }, presenting: { userId: 'u-maya', name: 'Maya Chen', view: VIEW, hold: true }, bring: { id: 'b-maya', view: VIEW }, hand: { id: 'h-maya', raisedAt: 1000 }, lower: [{ id: 'l-maya', client: sam.doc.clientID }] })
    // Sam may edit; his tab says he may not.
    sam.presence({ user: { id: 'u-sam', name: 'Sam Rivera', edit: false }, presenting: { userId: 'u-sam', name: 'Sam Rivera', view: VIEW, hold: true }, bring: { id: 'b-sam', view: VIEW }, lower: [{ id: 'l-sam', client: maya.doc.clientID }] })
    await until(() => sam.others.get(maya.doc.clientID)?.user && maya.others.get(sam.doc.clientID)?.user, 3000, 'each hears the other')

    // A reader keeps her pointer and raises a hand, but nobody is held by, follows or is brought to her view, and her
    // word lowers no hand.
    const m = sam.others.get(maya.doc.clientID)
    assert.deepEqual({ edit: m.user.edit, presenting: m.presenting, bring: m.bring, lower: m.lower, pointer: m.pointer, hand: m.hand }, { edit: false, presenting: undefined, bring: undefined, lower: undefined, pointer: { x: 1, y: 2 }, hand: { id: 'h-maya', raisedAt: 1000 } })
    // An editor's presenting, hold, bring and lower reach the others, and his user says he may edit.
    const s = maya.others.get(sam.doc.clientID)
    assert.deepEqual({ edit: s.user.edit, presenting: s.presenting, bring: s.bring, lower: s.lower }, { edit: true, presenting: { userId: 'u-sam', name: 'Sam Rivera', view: VIEW, hold: true }, bring: { id: 'b-sam', view: VIEW }, lower: [{ id: 'l-sam', client: maya.doc.clientID }] })
})

test("a board member whose sign-in answer leaves canEdit out may only read: no writes, no presenting or bring, edit false (V-230)", async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const maya = await open(MAYA, 'unsaid')
    // Fail closed: only canEdit: true lets a member write; Hocuspocus refuses a read-only connection's writes.
    assert.equal(maya.scope, 'readonly')
    maya.presence({ user: { id: 'u-maya', name: 'Maya Chen', edit: true }, presenting: { userId: 'u-maya', name: 'Maya Chen', view: VIEW, hold: true }, bring: { id: 'b-maya', view: VIEW } })
    await until(() => sam.others.get(maya.doc.clientID)?.user, 3000, 'Sam hears Maya')
    const m = sam.others.get(maya.doc.clientID)
    assert.deepEqual({ edit: m.user.edit, presenting: m.presenting, bring: m.bring }, { edit: false, presenting: undefined, bring: undefined })
})

test("a tab speaks only for its own presence: nobody moves or removes another's cursor", async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const maya = await open(MAYA)
    const kai = await open(CALL_GUEST, 'edit')
    sam.presence({ user: { id: 'u-sam', name: 'Sam Rivera' }, pointer: { x: 1, y: 1 } })
    maya.presence({ user: { id: 'u-maya', name: 'Maya Chen' }, pointer: { x: 2, y: 2 } })
    await until(() => maya.others.get(sam.doc.clientID)?.pointer && sam.others.get(maya.doc.clientID)?.pointer, 3000, 'Sam and Maya hear each other')

    kai.presence({ user: { id: 'u-sam', name: 'Sam Rivera' }, pointer: { x: 999, y: 999 } }, { clientId: sam.doc.clientID, clock: 1000 })
    kai.presence(null, { clientId: maya.doc.clientID, clock: 1000 })
    // The guest's own, after: its messages were all handled once it is heard.
    kai.presence({ user: { name: 'Kai' }, pointer: { x: 3, y: 3 } })
    await until(() => maya.others.get(kai.doc.clientID)?.pointer, 3000, "Maya hears the guest's own")
    assert.deepEqual(maya.others.get(sam.doc.clientID).pointer, { x: 1, y: 1 })
    assert.deepEqual(sam.others.get(maya.doc.clientID)?.pointer, { x: 2, y: 2 })

    // Its own leaving is its own to say.
    kai.presence(null)
    await until(() => !maya.others.has(kai.doc.clientID), 3000, 'the guest leaves')
})

test("a client's broadcast reaches no other tab, and its socket stays open", async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const guest = await open(LINK_GUEST)
    guest.broadcast(JSON.stringify({ type: 'epoch-changed' }))
    guest.broadcast(JSON.stringify({ type: 'session', level: 'view', canEdit: false, locked: true, lineage: 'forged' }))
    // Sam says something after it, which the guest hears: the broadcasts had their turn.
    sam.presence({ user: { id: 'u-sam', name: 'Sam Rivera' } })
    await until(() => guest.others.get(sam.doc.clientID)?.user, 3000, 'the guest still hears the board')
    await sleep(200)
    assert.deepEqual(sam.stateless.filter((m) => /epoch-changed|forged/.test(m)), [])
    assert.equal(guest.ws.readyState, 1, "the guest's socket is open")
})

// A tab that reconnects keeps its client id, and the server holds the socket
// it left (a silent drop) until its ping check, 30-60 s later: an editor's
// reactions and Lowers reached guests that late (miro-24, DESIGN-BUILD-86,
// 2 Nov). The same person's new connection takes their client over at once
// (main's rule: an identity-checked takeover); nobody else's may.
test("a tab that reconnects speaks for its presence at once, on a board and on a doc, while the server still holds its silent old socket", async () => {
    for (const name of [BOARD, DOC]) {
        const { collab, open } = await room(name)
        const sam = await open(SAM)
        // Who hears Sam: a share link's guest on a board, Maya on a doc.
        const hears = await open(name === BOARD ? LINK_GUEST : MAYA)
        const id = sam.doc.clientID
        const said = (n) => ({ user: { id: 'u-sam', name: 'Sam Rivera' }, react: [{ id: `r${n}`, emoji: '👍' }] })
        const heard = (n) => !!hears.others.get(id)?.react?.some((r) => r.id === `r${n}`)
        sam.presence(said(1))
        await until(() => heard(1), 3000, `${name}: Sam's first reaction`)

        // The same tab on a new socket: its doc (so its client id) and its
        // awareness clock go on; the old socket stays open and says nothing.
        const again = new TestClient({ url: collab.url, name, token: SAM, doc: sam.doc })
        assert.equal(await again.connect(), 'synced')
        again.presenceClock = sam.presenceClock
        again.presence(said(2))
        await until(() => heard(2), 1000, `${name}: the reconnected tab's reaction within 1 s, its old socket still open`)
        assert.equal(sam.ws.readyState, 1, `${name}: the old socket is still open`)

        // The old socket's end takes nothing of the tab's presence with it;
        // the new socket's end takes it.
        await oldSocketEnds(collab, { old: sam, moves: [{ again, hears, heard, said }] })
        await again.close()
        await until(() => !hears.others.has(id), 3000, `${name}: Sam's presence goes with the tab's socket`)
    }
})

test("nobody else's connection takes over a held presence: another member's, a call guest's, or another visitor's of the same link", async () => {
    for (const name of [BOARD, DOC]) {
        const { open } = await room(name)
        const sam = await open(SAM)
        const maya = await open(MAYA)
        sam.presence({ user: { id: 'u-sam', name: 'Sam Rivera' }, pointer: { x: 1, y: 1 } })
        await until(() => maya.others.get(sam.doc.clientID)?.pointer, 3000, `${name}: Maya hears Sam`)
        const claim = { user: { id: 'u-sam', name: 'Sam Rivera' }, pointer: { x: 999, y: 999 } }
        maya.presence(claim, { clientId: sam.doc.clientID, clock: 1000 })
        if (name === BOARD) {
            const kai = await open(CALL_GUEST, 'edit')
            kai.presence(claim, { clientId: sam.doc.clientID, clock: 1001 })
            kai.presence({ user: { name: 'Kai' }, pointer: { x: 3, y: 3 } })
            await until(() => maya.others.get(kai.doc.clientID)?.pointer, 3000, "Maya hears the call guest's own")
            // One link's visitors all sign in as the link: a link names no one
            // person, so one visitor never takes over another's presence.
            const first = await open(LINK_GUEST)
            first.presence({ user: { name: 'Guest' }, react: [{ id: 'mine', emoji: '👍' }] })
            await until(() => sam.others.get(first.doc.clientID)?.react, 3000, 'Sam hears the first visitor')
            const second = await open(LINK_GUEST)
            second.presence({ user: { name: 'Guest' }, react: [{ id: 'forged', emoji: '👎' }] }, { clientId: first.doc.clientID, clock: 1000 })
            second.presence({ user: { name: 'Guest' } })
            await until(() => sam.others.has(second.doc.clientID), 3000, "Sam hears the second visitor's own")
            assert.deepEqual(sam.others.get(first.doc.clientID).react, [{ id: 'mine', emoji: '👍' }])
        }
        // Maya's own, after: her claim was handled once it is heard.
        maya.presence({ user: { id: 'u-maya', name: 'Maya Chen' }, pointer: { x: 2, y: 2 } })
        await until(() => sam.others.get(maya.doc.clientID)?.pointer, 3000, `${name}: Sam hears Maya's own`)
        assert.deepEqual(maya.others.get(sam.doc.clientID).pointer, { x: 1, y: 1 }, name)
    }
})

// A call's guest signs in as their own person (their token's sub; its call
// claim says it is a call's), so their tab's reconnect takes their presence
// over at once too, as a member's does (main's rule, 2 Nov). A share link's
// visitors all sign in as the link: one visitor never takes another's over
// (above), and a visitor's own tab does, by its key (below).
test("a call's guest whose tab reconnects speaks for their presence at once, and the old socket's end takes nothing with it", async () => {
    const { collab, open } = await room(BOARD)
    const sam = await open(SAM)
    const kai = await open(CALL_GUEST, 'edit')
    const id = kai.doc.clientID
    const said = (n) => ({ user: { name: 'Kai' }, pointer: { x: n, y: n }, react: [{ id: `k${n}`, emoji: '👋' }] })
    const heard = (n) => !!sam.others.get(id)?.react?.some((r) => r.id === `k${n}`)
    kai.presence(said(1))
    await until(() => heard(1), 3000, "the call guest's first reaction")

    const again = new TestClient({ url: collab.url, name: BOARD, token: CALL_GUEST, doc: kai.doc })
    assert.equal(await again.connect(), 'synced')
    again.presenceClock = kai.presenceClock
    again.presence(said(2))
    await until(() => heard(2), 1000, "the call guest's reconnected tab within 1 s, its old socket still open")
    assert.deepEqual(sam.others.get(id).pointer, { x: 2, y: 2 }, 'their pointer rides along')
    assert.equal(kai.ws.readyState, 1, 'the old socket is still open')

    await oldSocketEnds(collab, { old: kai, moves: [{ again, hears: sam, heard, said }] })
    await again.close()
    await until(() => !sam.others.has(id), 3000, "the call guest's presence goes with the tab's socket")
})

// "Guests can draw" turned on or off closes the guest's socket (reauth). Their
// tab asks for a new board token with the call token it joined with, and every
// token issued from it names the same identity, so the guest stays the same
// person: their tab takes its presence back at once, from the old socket or
// after it has ended. A guest who joined by the same meeting link has an
// identity of their own (guest-<grant>-<their own suffix>) and takes nothing.
test("a call's guest whose board token is re-issued (the same identity) speaks for their presence at once; another guest of the same link takes nothing", async () => {
    const { backend, collab, open } = await room(BOARD)
    const sam = await open(SAM)
    const kai = await open(CALL_GUEST, 'view')
    const id = kai.doc.clientID
    const said = (n) => ({ user: { name: 'Kai' }, react: [{ id: `k${n}`, emoji: '👋' }] })
    const heard = (n) => !!sam.others.get(id)?.react?.some((r) => r.id === `k${n}`)
    const reissued = (iat) => JSON.stringify({ ...JSON.parse(CALL_GUEST), iat })
    kai.presence(said(1))
    await until(() => heard(1), 3000, "Sam hears the call guest")

    backend.levels.set('guest-kai', 'edit')
    const again = new TestClient({ url: collab.url, name: BOARD, token: reissued(2), doc: kai.doc })
    assert.equal(await again.connect(), 'synced')
    again.presenceClock = kai.presenceClock
    again.presence(said(2))
    await until(() => heard(2), 1000, "the guest's tab on a re-issued token within 1 s, its old socket still open")
    await kai.close()
    await again.close()
    await until(() => !sam.others.has(id), 3000, "the call guest's presence goes with the tab's socket")

    const third = new TestClient({ url: collab.url, name: BOARD, token: reissued(3), doc: kai.doc })
    assert.equal(await third.connect(), 'synced')
    third.presenceClock = again.presenceClock
    third.presence(said(3))
    await until(() => heard(3), 1000, "the guest's tab on another re-issued token within 1 s, after its socket ended")

    const zoe = await open('{"guest":true,"sub":"guest-zoe","name":"Guest: Zoe","call":"s-1"}', 'edit')
    zoe.presence({ user: { name: 'Zoe' }, react: [{ id: 'forged', emoji: '👎' }] }, { clientId: id, clock: 1000 })
    zoe.presence({ user: { name: 'Zoe' } })
    await until(() => sam.others.has(zoe.doc.clientID), 3000, "Sam hears the other guest's own")
    assert.deepEqual(sam.others.get(id).react, [{ id: 'k3', emoji: '👋' }], 'the other guest of the same link takes nothing')
})

// A share link's visitors all sign in as the link, so a visitor's tab names
// itself with a key of its own on every socket it opens (random, made once per
// tab: lib/collabSocket.ts). The same tab, by its key, takes its presence over
// at once when it reconnects, as a member's does (V-337); a socket with
// another key, or none, is another visitor's, whether the tab's presence is
// held or its socket has just ended.
test("a link visitor's tab that reconnects with its key speaks for its presence at once, on a board and on a doc; another key, or none, takes nothing", async () => {
    const newKey = () => randomBytes(16).toString('hex')
    for (const name of [BOARD, DOC]) {
        const { collab, open } = await room(name)
        const sam = await open(SAM)
        const visitor = async (tab, doc) => {
            const t = new TestClient({ url: collab.url, name, token: LINK_GUEST, tab, doc })
            assert.equal(await t.connect(), 'synced')
            return t
        }
        const key = newKey()
        const first = await visitor(key)
        const id = first.doc.clientID
        const said = (n) => ({ user: { name: 'Guest' }, react: [{ id: `v${n}`, emoji: '👍' }] })
        const heard = (n) => !!sam.others.get(id)?.react?.some((r) => r.id === `v${n}`)
        first.presence(said(1))
        await until(() => heard(1), 3000, `${name}: the visitor's first reaction`)

        // Another visitor's socket says it is this tab (its client id, a newer
        // entry): with a key of its own, or none, it takes nothing over.
        const others = [await visitor(newKey()), await visitor()]
        for (const [n, other] of others.entries()) {
            other.presence(said(`x${n}`), { clientId: id, clock: 1000 + n })
            other.presence({ user: { name: 'Guest' } })
            await until(() => sam.others.has(other.doc.clientID), 3000, `${name}: Sam hears visitor ${n}'s own`)
        }
        assert.ok(heard(1) && !heard('x0') && !heard('x1'), `${name}: the held presence is the visitor's own`)

        // The same tab on a new socket, with its key: its doc (so its client
        // id) and its awareness clock go on; the old socket stays open.
        const again = await visitor(key, first.doc)
        again.presenceClock = first.presenceClock
        again.presence(said(2))
        await until(() => heard(2), 1000, `${name}: the visitor's reconnected tab within 1 s, its old socket still open`)
        assert.equal(first.ws.readyState, 1, `${name}: the old socket is still open`)

        // The old socket's end takes nothing with it; the new one's takes it.
        await oldSocketEnds(collab, { old: first, moves: [{ again, hears: sam, heard, said }] })
        await again.close()
        await until(() => !sam.others.has(id), 3000, `${name}: the visitor's presence goes with the tab's socket`)

        // Its socket ended: another key, or none, binds nothing; the tab's key
        // binds it back.
        for (const [n, other] of others.entries()) {
            other.presence(said(`y${n}`), { clientId: id, clock: 2000 + n })
            other.presence({ user: { name: 'Guest' }, react: [{ id: `own${n}`, emoji: '👋' }] })
            await until(() => sam.others.get(other.doc.clientID)?.react, 3000, `${name}: Sam hears visitor ${n}'s own again`)
        }
        assert.ok(!sam.others.has(id), `${name}: nobody else binds the visitor's ended presence`)
        const back = await visitor(key, first.doc)
        back.presenceClock = again.presenceClock
        back.presence(said(3))
        await until(() => heard(3), 1000, `${name}: the visitor's tab binds its presence back within 1 s`)
    }
})

// A tab carries every doc it opens on one socket (lib/collabSocket.ts), each
// doc with a client of its own, so one socket speaks for one client per doc: a
// board's and a doc's presence ride it side by side. When the tab reconnects,
// its new socket carries both docs and takes both over, and the old socket's
// end takes nothing from either (V-348).
test("one socket carrying a board and a doc speaks for each doc's own client, and a reconnect takes both over", async () => {
    const { collab } = await room()
    const tab = async (name, token, { doc, via } = {}) => {
        const t = new TestClient({ url: collab.url, name, token, doc, via })
        assert.equal(await t.connect(), 'synced')
        return t
    }
    // Sam on each doc, on a socket each; Maya's tab with both docs on one.
    const sam = { [BOARD]: await tab(BOARD, SAM), [DOC]: await tab(DOC, SAM) }
    const board = await tab(BOARD, MAYA)
    const doc = await tab(DOC, MAYA, { via: board })
    assert.equal(doc.ws, board.ws, "the doc rides the board's socket")
    assert.notEqual(doc.doc.clientID, board.doc.clientID, 'each doc has a client of its own')
    const said = (n) => ({ user: { id: 'u-maya', name: 'Maya Chen' }, react: [{ id: `m${n}`, emoji: '🎉' }] })
    const heard = (maya, n) => !!sam[maya.name].others.get(maya.doc.clientID)?.react?.some((r) => r.id === `m${n}`)
    for (const maya of [board, doc]) maya.presence(said(1))
    await until(() => heard(board, 1) && heard(doc, 1), 3000, "Sam hears Maya's own client on the board and on the doc")

    // The tab reconnects: one new socket carries both docs, whose clients and
    // clocks go on; the old socket stays open and says nothing.
    const board2 = await tab(BOARD, MAYA, { doc: board.doc })
    const doc2 = await tab(DOC, MAYA, { doc: doc.doc, via: board2 })
    for (const [was, now] of [[board, board2], [doc, doc2]]) {
        now.presenceClock = was.presenceClock
        now.presence(said(2))
    }
    await until(() => heard(board, 2) && heard(doc, 2), 1000, "the new socket speaks for Maya on both within 1 s")

    await oldSocketEnds(collab, {
        old: board,
        riding: [doc],
        moves: [board2, doc2].map((again, i) => ({ again, hears: sam[again.name], heard: (n) => heard([board, doc][i], n), said })),
    })
})

test('a sign-in names one person: a member by their id, a call\'s guest by their own sub; a share link names no one', () => {
    assert.equal(personOf({ sub: 'u-sam' }), 'member:u-sam')
    assert.equal(personOf({ guest: true, sub: 'guest-g1-x7', call: 's-1' }), 'call-guest:guest-g1-x7')
    // Every visitor of one link signs in as guest-<grant>.
    assert.equal(personOf({ guest: true, sub: 'guest-g1' }), '')
    assert.equal(personOf({ guest: true, sub: 'guest-g1', call: '' }), '')
    assert.equal(personOf({ guest: true, sub: 'guest-g1', call: 7 }), '')
    // A token that names nobody names no person.
    assert.equal(personOf({ sub: '' }), '')
    assert.equal(personOf({}), '')
    assert.equal(personOf(null), '')
    // A member and a guest are never one person, whatever their subs.
    assert.notEqual(personOf({ sub: 'x' }), personOf({ guest: true, sub: 'x', call: 's-1' }))
})

test("a link visitor's tab names one person by its key: 32 hex characters, its own, on its link", () => {
    const key = randomBytes(16).toString('hex')
    const link = { guest: true, sub: 'guest-g1' }
    assert.equal(personOf(link, key), `link-guest:guest-g1:${key}`)
    // Another key, another link: another person.
    assert.notEqual(personOf(link, randomBytes(16).toString('hex')), personOf(link, key))
    assert.notEqual(personOf({ guest: true, sub: 'guest-g2' }, key), personOf(link, key))
    // Anything but 32 lowercase hex characters names no one.
    for (const bad of [undefined, null, '', 7, key.slice(1), `${key}0`, key.toUpperCase(), `${key.slice(2)}zz`, [key]]) {
        assert.equal(personOf(link, bad), '', `tab ${JSON.stringify(bad)}`)
    }
    // A member and a call's guest are who they are, whatever key their tab sends.
    assert.equal(personOf({ sub: 'u-sam' }, key), 'member:u-sam')
    assert.equal(personOf({ guest: true, sub: 'guest-g1-x7', call: 's-1' }, key), 'call-guest:guest-g1-x7')
})
