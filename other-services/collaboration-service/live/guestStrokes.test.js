import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import { FakeBackend, FakeRedis, TestClient, onClose, startServer, until } from '../testkit.js'
import { createLiveServer } from './server.js'
import { GUEST_AUTHORS } from './ops/guestStrokes.js'

// A call's guests drawing on the board on its stage (calls package B,
// PROGRAM.md "Guest strokes"): this service alone records which elements a
// guest changed last (guestAuthors), and nobody's socket can forge or erase
// that record; "Guests can draw" keeps the board as it was (guestsSnapshot),
// and "Undo guests' drawing" (undoGuests) takes back only what guests did
// since: what they drew goes, what they changed returns as it was, and what
// members did stays.

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

const ID = '3c9e1f2a-4b5d-4e6f-9a7b-8c9d0e1f2a3b'
const NAME = `board:${ID}`
const SECRET = 'test-secret'
const GUEST = '{"guest":true,"sub":"guest-g-1","name":"Guest: Gil","call":"s-1"}'

async function world() {
    const backend = new FakeBackend()
    backend.boards.set(ID, { board_state: '' })
    backend.levels.set('guest-g-1', 'edit') // "Guests can draw" is on
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const live = createLiveServer({ getSecret: () => SECRET, hocuspocus: collab.server.hocuspocus, deps: collab.live })
    await new Promise((resolve) => live.listen(0, '127.0.0.1', resolve))
    const stopLive = onClose(() => new Promise((resolve) => {
        live.close(resolve)
        live.closeAllConnections()
    }))
    const base = `http://127.0.0.1:${live.address().port}`
    const op = async (name, payload) => {
        const res = await fetch(`${base}/live/boards/${ID}/${name}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'X-Internal-Secret': SECRET },
            body: JSON.stringify(payload),
        })
        return { status: res.status, ...(await res.json()) }
    }
    const tabs = []
    const open = async (token) => {
        const tab = new TestClient({ url: collab.url, name: NAME, token })
        assert.equal(await tab.connect(), 'synced')
        tabs.push(tab)
        return tab
    }
    const doc = () => collab.server.hocuspocus.documents.get(NAME)
    const close = async () => {
        for (const tab of tabs) {
            await tab.close()
            tab.destroy()
        }
        await stopLive()
        await collab.stop()
        assert.deepEqual(rejections, [])
    }
    return { op, open, doc, close }
}

const el = (id, x, version = 1) => ({ id, type: 'rectangle', x, y: 0, version, versionNonce: version, isDeleted: false })
const draw = (tab, e) => tab.doc.getMap('elements').set(e.id, e)
const has = (w, id, pred = () => true) => () => {
    const v = w.doc()?.getMap('elements').get(id)
    return v !== undefined && pred(v)
}

test("a guest's strokes are theirs by name until a member changes them; members' never are", async () => {
    const w = await world()
    const member = await w.open('{"sub":"sam"}')
    const guest = await w.open(GUEST)
    draw(guest, el('e-g', 10))
    draw(member, el('e-m', 20))
    await until(has(w, 'e-g'), 2000, "the guest's stroke")
    await until(has(w, 'e-m'), 2000, "the member's stroke")
    const authors = () => w.doc().getMap(GUEST_AUTHORS)
    await until(() => authors().has('e-g'), 2000, "the guest's name on their stroke")
    assert.equal(authors().get('e-g').name, 'Guest: Gil')
    assert.equal(authors().get('e-g').id, 'guest-g-1')
    assert.equal(authors().has('e-m'), false)

    draw(member, el('e-g', 30, 2))
    await until(() => !authors().has('e-g'), 2000, 'the member taking the stroke over')
    await w.close()
})

test("nobody's socket forges or erases who drew what", async () => {
    const w = await world()
    const member = await w.open('{"sub":"sam"}')
    const guest = await w.open(GUEST)
    draw(guest, el('e-g', 10))
    const authors = () => w.doc().getMap(GUEST_AUTHORS)
    await until(() => authors().has('e-g'), 2000, "the guest's name on their stroke")

    // A guest blames a member's stroke on someone else, and erases their own.
    draw(member, el('e-m', 20))
    await until(has(w, 'e-m'), 2000, "the member's stroke")
    guest.doc.getMap(GUEST_AUTHORS).set('e-m', { id: 'guest-other', name: 'Guest: Not me' })
    guest.doc.getMap(GUEST_AUTHORS).delete('e-g')
    // A member erases the guest's name.
    member.doc.getMap(GUEST_AUTHORS).delete('e-g')
    draw(member, el('e-check', 1))
    await until(has(w, 'e-check'), 2000, 'a later change')
    await until(() => !authors().has('e-m') && authors().get('e-g')?.id === 'guest-g-1', 2000, 'the record put back')
    await w.close()
})

test("undoing guests' drawing takes back only what guests did since they could draw", async () => {
    const w = await world()
    const member = await w.open('{"sub":"sam"}')
    draw(member, el('e-1', 0))
    draw(member, el('e-2', 5))
    await until(has(w, 'e-2'), 2000, 'the board before guests draw')

    const kept = await w.op('guestsSnapshot', {})
    assert.equal(kept.status, 200, kept.msg)
    assert.ok(kept.data.state.length > 0)
    assert.equal(kept.data.elements, 2)
    assert.equal((await w.op('guestsBegin', {})).status, 200)

    const guest = await w.open(GUEST)
    draw(guest, el('e-g', 40)) // drew
    draw(guest, el('e-1', 99, 2)) // moved a member's
    draw(guest, { ...el('e-2', 5, 2), isDeleted: true }) // erased a member's
    draw(member, el('e-m', 70)) // a member's work since
    await until(has(w, 'e-m'), 2000, "the member's stroke")
    await until(() => w.doc().getMap(GUEST_AUTHORS).size === 3, 2000, "the guests' record")

    const undone = await w.op('undoGuests', { state: kept.data.state })
    assert.equal(undone.status, 200, undone.msg)
    assert.equal(undone.data.undone, 3)
    const elements = w.doc().getMap('elements')
    assert.equal(elements.get('e-g').isDeleted, true)
    assert.equal(elements.get('e-1').x, 0)
    assert.ok(elements.get('e-1').version > 2, 'a restored element wins over the guest version')
    assert.equal(elements.get('e-2').isDeleted, false)
    assert.equal(elements.get('e-m').isDeleted, false)
    assert.equal(w.doc().getMap(GUEST_AUTHORS).size, 0)
    // The tabs see it too.
    await until(() => member.doc.getMap('elements').get('e-g')?.isDeleted === true, 2000, "the member's tab")

    const nothing = await w.op('undoGuests', { state: '' })
    assert.equal(nothing.status, 400)
    await w.close()
})

// Keeping the version changes nothing: the record of guests' strokes so far
// goes only once the API has stored it (guestsBegin), so a version that
// couldn't be stored loses nothing.
test("keeping the version leaves the record; beginning afresh clears it", async () => {
    const w = await world()
    const guest = await w.open(GUEST)
    draw(guest, el('e-g', 10))
    const authors = () => w.doc().getMap(GUEST_AUTHORS)
    await until(() => authors().has('e-g'), 2000, "the guest's name on their stroke")
    const kept = await w.op('guestsSnapshot', {})
    assert.equal(kept.status, 200, kept.msg)
    assert.equal(authors().has('e-g'), true)
    const begun = await w.op('guestsBegin', {})
    assert.equal(begun.status, 200, begun.msg)
    assert.equal(authors().size, 0)
    await w.close()
})
