// Echoes, checked: a real tab runs @hocuspocus/provider 3.4.3, whose
// awarenessUpdateHandler sends EVERY changed client back to the server,
// whatever the change's origin, so a tab echoes the updates and removals it
// hears from others, at the server's clock. TestClient never echoes, so these
// cases send the echo by hand (from CODE-REVIEWER-177's probe, 2 Nov). The
// relay's takeover (presenceGuard.js claim) must never move or bind a client
// on an echo, and nobody else's claim of a free client may lock its owner out.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { FakeBackend, FakeRedis, TestClient, startServer, until, sleep } from './testkit.js'

const BOARD_ID = '5b0f7a52-3c1e-4d8a-9f6b-2a7c4e1d9b30'
const BOARD = `board:${BOARD_ID}`
const DOC = 'c41f0a9e-6b2d-4e7a-8c15-3f9d2b7e6a10'
const SAM = '{"sub":"u-sam"}'
const MAYA = '{"sub":"u-maya"}'
const LINK_GUEST = '{"guest":true,"sub":"g-link","name":"Guest"}'
// A call's guest, as their board token names them: their call identity
// (guest-<grant>-<their own suffix>) and the call. The tab asks for a new
// token on every connect (CallGuestBoardStage's tokenFetcher), with the same
// call token, so the new one names the same identity.
const CALL_GUEST = '{"guest":true,"sub":"guest-g1-k41","name":"Guest: Kai Lin","call":"s-1"}'
const CALL_GUEST_REISSUED = '{"guest":true,"sub":"guest-g1-k41","name":"Guest: Kai Lin","call":"s-1","iat":2}'
// A link visitor's tab names itself by a key of its own on every socket (V-337).
const TAB_KEY = '3f9a0c4e7b21d58a6e0f4c3b9d7a2e15'

async function room(name) {
    const backend = new FakeBackend()
    backend.boards.set(BOARD_ID, { board_state: '' })
    backend.doc(DOC, '<p>hello</p>')
    backend.names.set('u-sam', 'Sam Rivera')
    backend.names.set('u-maya', 'Maya Chen')
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const open = async (token, tab) => {
        const client = new TestClient({ url: collab.url, name, token, tab })
        assert.equal(await client.connect(), 'synced')
        return client
    }
    return { backend, collab, open }
}

// Every socket on a doc closes (a reauth closes them all), so the doc unloads,
// and its tabs come back to a fresh copy that has no record of whose client
// is whose.
async function closeEvery(collab, name, ...tabs) {
    for (const tab of tabs) await tab.close()
    await until(() => !collab.server.hocuspocus.documents.has(name), 3000, `${name.slice(0, 5)} unloads once every socket has closed`)
}

const said = (n) => ({ user: { id: 'u-sam', name: 'Sam Rivera' }, react: [{ id: `r${n}`, emoji: '👍' }] })

// E1: Sam's socket ends (a clean close, or the ping check closing a silent one
// after a laptop sleeps); the server removes Sam's presence and tells every tab.
// Another tab's provider echoes that removal (Sam's id, the same clock, null).
// Then Sam's same tab reconnects (same doc, so the same client id) and speaks.
for (const [name, echoer] of [[BOARD, MAYA], [DOC, MAYA], [BOARD, LINK_GUEST]]) {
    test(`E1 ${name.slice(0, 5)} echo by ${JSON.parse(echoer).sub}: another tab echoing Sam's removal never locks Sam's reconnected tab out`, async () => {
        const { collab, open } = await room(name)
        const sam = await open(SAM)
        const other = await open(echoer)
        const id = sam.doc.clientID
        const heard = (n) => !!other.others.get(id)?.react?.some((r) => r.id === `r${n}`)
        sam.presence(said(1))
        await until(() => heard(1), 3000, 'the other tab hears Sam')
        const clock = sam.presenceClock
        await sam.close()
        await until(() => !other.others.has(id), 3000, 'the other tab hears Sam leave')
        other.presence(null, { clientId: id, clock }) // the provider's echo of the removal it applied
        await sleep(200)
        const again = new TestClient({ url: collab.url, name, token: SAM, doc: sam.doc })
        assert.equal(await again.connect(), 'synced')
        again.presenceClock = clock
        again.presence(said(2))
        await until(() => heard(2), 3000, "Sam's reconnected tab is heard after the other tab's echo")
    })
}

// E2: Sam has two tabs open (each its own doc, so its own client id); Maya watches.
// Tab 2's provider echoes tab 1's update (tab 1's id, its clock, the state as the
// server sent it). Then tab 2 closes: tab 1, still open, keeps its presence.
for (const name of [BOARD, DOC]) {
    test(`E2 ${name.slice(0, 5)}: a second tab of the same person echoing the first's update takes nothing of it when it closes`, async () => {
        const { open } = await room(name)
        const tab1 = await open(SAM)
        const tab2 = await open(SAM)
        const maya = await open(MAYA)
        const id = tab1.doc.clientID
        tab1.presence({ user: { id: 'u-sam', name: 'Sam Rivera' }, pointer: { x: 5, y: 5 } })
        await until(() => maya.others.get(id)?.pointer && tab2.others.get(id)?.pointer, 3000, 'Maya and tab 2 hear tab 1')
        tab2.presence(tab2.others.get(id), { clientId: id, clock: tab1.presenceClock }) // the provider's echo
        await sleep(200)
        await tab2.close()
        await sleep(400)
        assert.ok(maya.others.has(id), "tab 1's presence outlives tab 2's close")
    })
}

// E3 (deliberate, a crafted client): Maya claims Sam's removed id at a clock of
// her choosing before Sam's tab reconnects. Expected to fail at base as well
// (clock inflation already froze the owner out of a free id there).
for (const name of [BOARD, DOC]) {
    test(`E3 ${name.slice(0, 5)}: another person's crafted claim of a removed id never locks its owner out`, async () => {
        const { collab, open } = await room(name)
        const sam = await open(SAM)
        const maya = await open(MAYA)
        const id = sam.doc.clientID
        const heard = (n) => !!maya.others.get(id)?.react?.some((r) => r.id === `r${n}`)
        sam.presence(said(1))
        await until(() => heard(1), 3000, 'Maya hears Sam')
        const clock = sam.presenceClock
        await sam.close()
        await until(() => !maya.others.has(id), 3000, 'Maya hears Sam leave')
        maya.presence({ user: { id: 'u-maya', name: 'Maya Chen' }, pointer: { x: 9, y: 9 } }, { clientId: id, clock: clock + 1 })
        await sleep(200)
        const again = new TestClient({ url: collab.url, name, token: SAM, doc: sam.doc })
        assert.equal(await again.connect(), 'synced')
        again.presenceClock = clock + 1
        again.presence(said(2))
        await until(() => heard(2), 3000, "Sam's reconnected tab is heard after Maya's crafted claim")
    })
}

// E3b: the same crafted claim at an inflated clock (1000): pre-existing at
// base, where a free id took any writer and the higher clock froze the owner out.
for (const name of [BOARD, DOC]) {
    test(`E3b ${name.slice(0, 5)}: another person's crafted claim of a removed id at clock 1000 never locks its owner out`, async () => {
        const { collab, open } = await room(name)
        const sam = await open(SAM)
        const maya = await open(MAYA)
        const id = sam.doc.clientID
        const heard = (n) => !!maya.others.get(id)?.react?.some((r) => r.id === `r${n}`)
        sam.presence(said(1))
        await until(() => heard(1), 3000, 'Maya hears Sam')
        const clock = sam.presenceClock
        await sam.close()
        await until(() => !maya.others.has(id), 3000, 'Maya hears Sam leave')
        maya.presence({ user: { id: 'u-maya', name: 'Maya Chen' }, pointer: { x: 9, y: 9 } }, { clientId: id, clock: 1000 })
        await sleep(200)
        const again = new TestClient({ url: collab.url, name, token: SAM, doc: sam.doc })
        assert.equal(await again.connect(), 'synced')
        again.presenceClock = clock
        again.presence(said(2))
        await until(() => heard(2), 3000, "Sam's reconnected tab is heard after Maya's crafted claim")
    })
}

// E2b: tab 2's y-protocols times tab 1's state out (a background tab's renewals
// are throttled past 30 s) and its provider echoes the removal (tab 1's id, the
// same clock, null). Nothing closes: tab 1's presence stays for everyone.
for (const name of [BOARD, DOC]) {
    test(`E2b ${name.slice(0, 5)}: a second tab of the same person echoing a timed-out removal of the first's leaves it in place`, async () => {
        const { open } = await room(name)
        const tab1 = await open(SAM)
        const tab2 = await open(SAM)
        const maya = await open(MAYA)
        const id = tab1.doc.clientID
        tab1.presence({ user: { id: 'u-sam', name: 'Sam Rivera' }, pointer: { x: 5, y: 5 } })
        await until(() => maya.others.get(id)?.pointer && tab2.others.get(id)?.pointer, 3000, 'Maya and tab 2 hear tab 1')
        tab2.presence(null, { clientId: id, clock: tab1.presenceClock })
        await sleep(400)
        assert.ok(maya.others.has(id), "tab 1's presence stays after tab 2's echoed removal")
    })
}

// E4: a share link's visitor has no person recorded, so only the clock rule
// keeps another tab's echo of their removal (the same clock, null) from
// binding their free client; bound to that tab, the visitor's own reconnect
// would be refused for as long as it stays (CODE-REVIEWER-177, row 2351).
for (const name of [BOARD, DOC]) {
    test(`E4 ${name.slice(0, 5)}: another tab echoing a link visitor's removal never locks the visitor's reconnected tab out`, async () => {
        const { collab, open } = await room(name)
        const visitor = await open(LINK_GUEST)
        const maya = await open(MAYA)
        const id = visitor.doc.clientID
        const reacted = (n) => ({ user: { name: 'Guest' }, react: [{ id: `v${n}`, emoji: '👍' }] })
        const heard = (n) => !!maya.others.get(id)?.react?.some((r) => r.id === `v${n}`)
        visitor.presence(reacted(1))
        await until(() => heard(1), 3000, 'Maya hears the visitor')
        const clock = visitor.presenceClock
        await visitor.close()
        await until(() => !maya.others.has(id), 3000, 'Maya hears the visitor leave')
        maya.presence(null, { clientId: id, clock }) // the provider's echo of the removal it applied
        await sleep(200)
        const again = new TestClient({ url: collab.url, name, token: LINK_GUEST, doc: visitor.doc })
        assert.equal(await again.connect(), 'synced')
        again.presenceClock = clock
        again.presence(reacted(2))
        await until(() => heard(2), 3000, "the visitor's reconnected tab is heard after Maya's echo")
    })
}

// E5 (2 Nov, DESIGN-BUILD-89's r8 walk on the demo): "Guests can draw" closes
// every socket on the board (a reauth), so the board unloads and each tab
// comes back to a fresh copy. The host's tab reconnects first, and its
// provider echoes the guest's removal (the guest's id, at the clock it last
// heard, null): it removed everyone else's presence when its socket closed and
// sends that on once connected, before or after its own (startSync's). On the
// demo that echo bound the guest's client to the host's socket, and none of
// the guest's next 42 entries reached anyone. The guest's tab reconnects with
// the same doc (so the same client id) and its token re-issued, re-sends its
// presence at its clock (startSync), then speaks: the host hears it within
// 1 s. The same holds for a member on a doc and for a link visitor by its key.
const KINDS = [
    // who, the doc, their token, its re-issued copy, their tab's key, their level before and after
    ['a call guest', BOARD, CALL_GUEST, CALL_GUEST_REISSUED, undefined, ['view', 'edit']],
    ['a member', DOC, MAYA, MAYA, undefined, []],
    ['a link visitor', BOARD, LINK_GUEST, LINK_GUEST, TAB_KEY, []],
]
for (const [who, name, token, reissued, tab, [before, after]] of KINDS) {
    for (const order of ['after', 'before']) {
        test(`E5 ${who} on ${name.slice(0, 5)}, the host echoing their removal ${order} its own: when the doc reloads, the echo never locks their reconnected tab out`, async () => {
            const { backend, collab, open } = await room(name)
            const sub = JSON.parse(token).sub
            if (before) backend.levels.set(sub, before)
            const sam = await open(SAM)
            const guest = await open(token, tab)
            const id = guest.doc.clientID
            const theirs = (n) => ({ user: { name: who }, react: [{ id: `g${n}`, emoji: '👋' }] })
            const heard = (by, n) => !!by.others.get(id)?.react?.some((r) => r.id === `g${n}`)
            sam.presence(said(1))
            guest.presence(theirs(1))
            await until(() => heard(sam, 1), 3000, 'Sam hears them')
            const clock = guest.presenceClock

            if (after) backend.levels.set(sub, after) // the presenter lets guests draw
            await closeEvery(collab, name, guest, sam)
            const host = new TestClient({ url: collab.url, name, token: SAM, doc: sam.doc })
            assert.equal(await host.connect(), 'synced')
            const echo = () => host.presence(null, { clientId: id, clock })
            if (order === 'before') echo()
            host.presence(said(1), { clock: sam.presenceClock }) // its own, at its clock (startSync)
            if (order === 'after') echo()
            await sleep(200)

            const back = new TestClient({ url: collab.url, name, token: reissued, tab, doc: guest.doc })
            assert.equal(await back.connect(), 'synced')
            back.presenceClock = clock
            back.presence(theirs(2), { clock }) // its own, at its clock (startSync)
            back.presence(theirs(3))
            await until(() => heard(host, 3), 1000, "the host hears their reconnected tab within 1 s")
            assert.ok(back.others.get(sam.doc.clientID)?.react?.some((r) => r.id === 'r1'), "they hear the host's own")
        })
    }
}

// E6: once the doc has reloaded, a tab that has spoken for its own client says
// something for another's at a clock above anything heard (an echo of a state
// its provider kept, or a crafted claim): it binds nothing, and the other's
// tab is heard within 1 s when it comes back.
for (const name of [BOARD, DOC]) {
    test(`E6 ${name.slice(0, 5)}: after the doc reloads, a tab that spoke for its own client binds no other's`, async () => {
        const { collab, open } = await room(name)
        const sam = await open(SAM)
        const maya = await open(MAYA)
        const id = maya.doc.clientID
        const hers = (n) => ({ user: { id: 'u-maya', name: 'Maya Chen' }, react: [{ id: `m${n}`, emoji: '🎉' }] })
        const heard = (by, n) => !!by.others.get(id)?.react?.some((r) => r.id === `m${n}`)
        sam.presence(said(1))
        maya.presence(hers(1))
        await until(() => heard(sam, 1), 3000, 'Sam hears Maya')
        const clock = maya.presenceClock

        await closeEvery(collab, name, maya, sam)
        const host = new TestClient({ url: collab.url, name, token: SAM, doc: sam.doc })
        assert.equal(await host.connect(), 'synced')
        host.presence(said(1), { clock: sam.presenceClock })
        host.presence(hers(1), { clientId: id, clock: clock + 5 })
        await sleep(200)

        const back = new TestClient({ url: collab.url, name, token: MAYA, doc: maya.doc })
        assert.equal(await back.connect(), 'synced')
        back.presenceClock = clock
        back.presence(hers(2), { clock })
        back.presence(hers(3))
        await until(() => heard(host, 3), 1000, "the host hears Maya's reconnected tab within 1 s")
    })
}
