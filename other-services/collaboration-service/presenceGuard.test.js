// Presence and broadcasts, checked on their way in.
//
// Presence (awareness) is every tab's say of who it is and where it points;
// Hocuspocus passes any connection's on to every tab on the doc, read-only or
// not, and passes any client's broadcast on as if the server had said it. So a
// tab could put any name on its cursor, steer whoever follows a presenter, or
// tell every tab to drop its copy of the doc. Now the server decides:
//   - who a tab is comes from its sign-in: a member's name as the API answers
//     it (never an address);
//   - a guest (a share link's, who may only look) publishes nothing;
//   - a member who may only read keeps their presence (a doc's "viewing" list);
//   - a tab speaks only for its own presence;
//   - a client's broadcast reaches no other tab, and its socket stays open.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { FakeBackend, FakeRedis, TestClient, startServer, until, sleep } from './testkit.js'

const BOARD_ID = '5b0f7a52-3c1e-4d8a-9f6b-2a7c4e1d9b30'
const BOARD = `board:${BOARD_ID}`
const DOC = 'c41f0a9e-6b2d-4e7a-8c15-3f9d2b7e6a10'
const VIEW = { scrollX: 0, scrollY: 0, zoom: 1, width: 800, height: 600 }
const COLOUR = /^#[0-9A-F]{6}$/

const SAM = '{"sub":"u-sam"}'
const MAYA = '{"sub":"u-maya"}'
const LEE = '{"sub":"u-lee"}'
// A share link's guest: guests only ever read here.
const LINK_GUEST = '{"guest":true,"sub":"g-link","name":"Guest"}'

async function room(name = BOARD) {
    const backend = new FakeBackend()
    backend.boards.set(BOARD_ID, { board_state: '' })
    backend.doc(DOC, '<p>hello</p>')
    backend.names.set('u-sam', 'Sam Rivera')
    backend.names.set('u-maya', 'Maya Chen')
    backend.names.set('u-lee', 'Lee Park')
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const open = async (token, level) => {
        if (level) backend.levels.set(JSON.parse(token).sub, level)
        const tab = new TestClient({ url: collab.url, name, token })
        assert.equal(await tab.connect(), 'synced')
        return tab
    }
    return { backend, collab, open }
}

test('a guest who may only look publishes nothing, whatever it sends', async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const guest = await open(LINK_GUEST)
    assert.equal(guest.scope, 'readonly')
    guest.presence({ user: { id: 'u-sam', name: 'Sam Rivera' }, pointer: { x: 1, y: 2 }, button: 'down', presenting: { userId: 'u-sam', name: 'Sam Rivera', view: VIEW } })
    await until(() => sam.others.has(guest.doc.clientID), 3000, "Sam hears the guest's client")
    assert.deepEqual(sam.others.get(guest.doc.clientID), {})
})

test("nobody wears another's name: a member is named by the API", async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const maya = await open(MAYA)
    maya.presence({ user: { id: 'u-sam', name: 'Sam Rivera', color: '#000000', hue: 'red', profileKey: 'k-maya' }, pointer: { x: 3, y: 4 }, button: 'up', mode: 'editing' })
    await until(() => sam.others.get(maya.doc.clientID)?.user, 3000, 'Sam hears Maya')

    const m = sam.others.get(maya.doc.clientID)
    assert.deepEqual({ id: m.user.id, name: m.user.name, profileKey: m.user.profileKey, hue: m.user.hue }, { id: 'u-maya', name: 'Maya Chen', profileKey: 'k-maya', hue: undefined })
    assert.match(m.user.color, COLOUR)
    assert.notEqual(m.user.color, '#000000')
    assert.deepEqual({ pointer: m.pointer, button: m.button, mode: m.mode }, { pointer: { x: 3, y: 4 }, button: 'up', mode: 'editing' })
})

test("a guest's presenting leaves nobody a presenter to follow; a member's says who presents", async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const maya = await open(MAYA)
    const kai = await open(LINK_GUEST)
    kai.presence({ user: { name: 'Kai Lin (guest)' }, pointer: { x: 1, y: 1 }, presenting: { userId: 'u-sam', name: 'Sam Rivera', view: VIEW } })
    maya.presence({ user: { id: 'u-maya', name: 'Maya Chen' }, presenting: { userId: 'u-sam', name: 'Sam Rivera', view: VIEW } })
    sam.presence({ user: { id: 'u-sam', name: 'Sam Rivera' }, pointer: { x: 7, y: 8 }, button: 'up', presenting: { userId: 'u-sam', name: 'Sam Rivera', view: VIEW } })
    await until(() => maya.others.has(kai.doc.clientID) && maya.others.get(sam.doc.clientID)?.user && sam.others.get(maya.doc.clientID)?.user, 3000, 'everyone heard')

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
        assert.deepEqual({ id: m.user.id, name: m.user.name, mode: m.mode, pointer: m.pointer }, { id: 'u-maya', name: 'Maya Chen', mode: 'viewing', pointer: { x: 9, y: 9 } }, name)
    }
})

test("a tab speaks only for its own presence: nobody moves or removes another's cursor", async () => {
    const { open } = await room()
    const sam = await open(SAM)
    const maya = await open(MAYA)
    const lee = await open(LEE)
    const guest = await open(LINK_GUEST)
    sam.presence({ user: { id: 'u-sam', name: 'Sam Rivera' }, pointer: { x: 1, y: 1 } })
    maya.presence({ user: { id: 'u-maya', name: 'Maya Chen' }, pointer: { x: 2, y: 2 } })
    await until(() => maya.others.get(sam.doc.clientID)?.pointer && sam.others.get(maya.doc.clientID)?.pointer, 3000, 'Sam and Maya hear each other')

    // A member, and a share link's guest, each try to move Sam's cursor and remove Maya's.
    for (const tab of [lee, guest]) {
        tab.presence({ user: { id: 'u-sam', name: 'Sam Rivera' }, pointer: { x: 999, y: 999 } }, { clientId: sam.doc.clientID, clock: 1000 })
        tab.presence(null, { clientId: maya.doc.clientID, clock: 1000 })
    }
    // Their own, after: their messages were all handled once each is heard.
    guest.presence({ user: { name: 'Kai' }, pointer: { x: 4, y: 4 } })
    lee.presence({ user: { name: 'Lee' }, pointer: { x: 3, y: 3 } })
    await until(() => maya.others.get(lee.doc.clientID)?.pointer && maya.others.has(guest.doc.clientID), 3000, "Maya hears each one's own")
    assert.deepEqual(maya.others.get(sam.doc.clientID).pointer, { x: 1, y: 1 })
    assert.deepEqual(sam.others.get(maya.doc.clientID)?.pointer, { x: 2, y: 2 })

    // Its own leaving is its own to say.
    lee.presence(null)
    await until(() => !maya.others.has(lee.doc.clientID), 3000, 'Lee leaves')
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
