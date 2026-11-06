import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import { FakeBackend, FakeRedis, TestClient, onClose, startServer, until } from '../testkit.js'
import { createLiveServer } from './server.js'
import { PROTECTED } from './ops/protect.js'
import { GUEST_AUTHORS } from './ops/guestStrokes.js'

// Protected objects on a board (miro-44, Miro's Protected Lock on every plan):
// the API's protect route alone locks and lifts them (protect), and this
// service puts back any socket's change to one, its bound text included,
// and any socket's write to the record of what is protected, owners' too:
// protection is lifted first, then the object is edited, as in Miro.

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

const ID = '7d1e2f3a-4b5c-4d6e-8f9a-0b1c2d3e4f5a'
const NAME = `board:${ID}`
const SECRET = 'test-secret'
const GUEST = '{"guest":true,"sub":"guest-g-1","name":"Guest: Gil","call":"s-1"}'
const SAM = { user_uuid: 'u-sam' }

async function world() {
    const backend = new FakeBackend()
    backend.boards.set(ID, { board_state: '' })
    backend.levels.set('guest-g-1', 'edit')
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

const el = (id, x, version = 1, more = {}) => ({ id, type: 'rectangle', x, y: 0, version, versionNonce: version, isDeleted: false, ...more })
const draw = (tab, e) => tab.doc.getMap('elements').set(e.id, e)
const at = (w, id) => w.doc()?.getMap('elements').get(id)
const has = (w, id, pred = () => true) => () => {
    const v = at(w, id)
    return v !== undefined && pred(v)
}
const protect = (w, ids, on = true) => w.op('protect', { element_ids: ids, protect: on, by: SAM, name: 'Sam' })

test('protecting locks the objects for every tab and records who did, with their bound text', async () => {
    const w = await world()
    const sam = await w.open('{"sub":"sam"}')
    draw(sam, el('e-1', 10, 3, { boundElements: [{ id: 't-1', type: 'text' }] }))
    draw(sam, el('t-1', 10, 1, { type: 'text', text: 'Budget', containerId: 'e-1' }))
    draw(sam, el('e-2', 50))
    await until(has(w, 't-1'), 2000, 'the board drawn')

    const res = await protect(w, ['e-1', 'gone'])
    assert.equal(res.status, 200)
    assert.equal(res.data.changed, 1)
    const record = w.doc().getMap(PROTECTED)
    assert.deepEqual([...record.keys()].sort(), ['e-1', 't-1'])
    assert.equal(record.get('e-1').by, 'u-sam')
    assert.equal(record.get('e-1').name, 'Sam')
    assert.equal(typeof record.get('e-1').at, 'number')
    for (const id of ['e-1', 't-1']) {
        assert.equal(at(w, id).locked, true)
        assert.ok(at(w, id).version > (id === 'e-1' ? 3 : 1), `${id} carries a newer version`)
    }
    assert.equal(at(w, 'e-2').locked, undefined, 'an object not named stays as it was')
    // Every tab takes the lock.
    await until(() => sam.doc.getMap('elements').get('e-1')?.locked === true, 2000, "the lock in Sam's tab")
    await w.close()
})

test("nobody's socket changes, deletes or unlocks a protected object or its text, its owner's included", async () => {
    const w = await world()
    const sam = await w.open('{"sub":"sam"}')
    const ed = await w.open('{"sub":"ed"}')
    const guest = await w.open(GUEST)
    draw(sam, el('e-1', 10, 1, { boundElements: [{ id: 't-1', type: 'text' }] }))
    draw(sam, el('t-1', 10, 1, { type: 'text', text: 'Budget', containerId: 'e-1' }))
    await until(has(w, 't-1'), 2000, 'the board drawn')
    assert.equal((await protect(w, ['e-1'])).status, 200)
    await until(() => ed.doc.getMap('elements').get('e-1')?.locked === true, 2000, "the lock in Ed's tab")

    const was = { ...at(w, 'e-1') }
    draw(ed, { ...was, x: 999, version: was.version + 5 })
    await until(() => ed.doc.getMap('elements').get('e-1')?.x === 10, 2000, "Ed's move put back in his own tab")
    draw(sam, { ...at(w, 'e-1'), locked: false, version: at(w, 'e-1').version + 1 })
    draw(guest, { ...at(w, 't-1'), text: 'Gone', version: at(w, 't-1').version + 1 })
    ed.doc.getMap('elements').delete('e-1')
    draw(ed, el('t-2', 10, 1, { type: 'text', text: 'Sneaked in', containerId: 'e-1' }))
    draw(ed, el('e-free', 5))
    await until(has(w, 'e-free'), 2000, 'a later change')

    await until(() => at(w, 'e-1')?.x === 10 && at(w, 'e-1')?.locked === true && at(w, 'e-1')?.isDeleted === false, 2000, 'the object put back')
    await until(() => at(w, 't-1')?.text === 'Budget', 2000, 'its text put back')
    await until(() => at(w, 't-2')?.isDeleted === true, 2000, 'new text on it taken off')
    assert.equal(at(w, 'e-free').isDeleted, false, 'an unprotected object is anyone with edit')
    for (const tab of [sam, ed, guest]) {
        await until(() => tab.doc.getMap('elements').get('e-1')?.x === 10 && tab.doc.getMap('elements').get('t-1')?.text === 'Budget', 2000, 'every tab put back')
    }
    await w.close()
})

test('text that was already on the board, bound onto a protected object, goes back to where it was (V-528)', async () => {
    const w = await world()
    const sam = await w.open('{"sub":"sam"}')
    const ed = await w.open('{"sub":"ed"}')
    draw(sam, el('e-1', 10, 1))
    draw(sam, el('t-3', 40, 1, { type: 'text', text: 'Loose words', containerId: null }))
    await until(has(w, 't-3'), 2000, 'the board drawn')
    assert.equal((await protect(w, ['e-1'])).status, 200)
    await until(() => ed.doc.getMap('elements').get('e-1')?.locked === true, 2000, "the lock in Ed's tab")

    // Ed's tab binds the free text into the protected shape: an update of an element that is not protected itself.
    const was = { ...at(w, 't-3') }
    draw(ed, { ...was, containerId: 'e-1', x: 10, version: was.version + 1 })
    draw(ed, el('e-free', 5))
    await until(has(w, 'e-free'), 2000, 'a later change')

    // It is put back as it was, free and whole (never deleted: it was someone's text before), in every tab.
    await until(() => at(w, 't-3')?.containerId === null && at(w, 't-3')?.x === 40, 2000, 'the re-bound text put back')
    assert.equal(at(w, 't-3').isDeleted, false, 'the text itself stays')
    assert.equal(at(w, 't-3').text, 'Loose words')
    assert.equal(w.doc().getMap(PROTECTED).has('t-3'), false, 'nothing new is protected')
    for (const tab of [sam, ed]) {
        await until(() => tab.doc.getMap('elements').get('t-3')?.containerId === null, 2000, 'every tab put back')
    }
    await w.close()
})

test("a protected object's deleted label brought back (an undo) goes back to deleted (V-528, CR-219 M1)", async () => {
    const w = await world()
    const sam = await w.open('{"sub":"sam"}')
    const ed = await w.open('{"sub":"ed"}')
    draw(sam, el('e-1', 10, 1, { boundElements: [{ id: 't-4', type: 'text' }] }))
    draw(sam, el('t-4', 10, 1, { type: 'text', text: 'Old label', containerId: 'e-1' }))
    await until(has(w, 't-4'), 2000, 'the board drawn')
    // The label is deleted before the shape is protected, so the record never holds it.
    draw(sam, { ...at(w, 't-4'), isDeleted: true, version: at(w, 't-4').version + 1 })
    await until(has(w, 't-4', (v) => v.isDeleted === true), 2000, 'the label deleted')
    assert.equal((await protect(w, ['e-1'])).status, 200)
    assert.equal(w.doc().getMap(PROTECTED).has('t-4'), false, 'a deleted label is not protected')
    await until(() => ed.doc.getMap('elements').get('e-1')?.locked === true, 2000, "the lock in Ed's tab")

    // Ed's undo brings the label back, bound to the protected shape as it was.
    draw(ed, { ...at(w, 't-4'), isDeleted: false, version: at(w, 't-4').version + 1 })
    draw(ed, el('e-free', 5))
    await until(has(w, 'e-free'), 2000, 'a later change')

    await until(() => at(w, 't-4')?.isDeleted === true, 2000, 'the label put back deleted')
    for (const tab of [sam, ed]) {
        await until(() => tab.doc.getMap('elements').get('t-4')?.isDeleted === true, 2000, 'every tab put back')
    }
    await w.close()
})

test("nobody's socket writes or erases the record of what is protected", async () => {
    const w = await world()
    const sam = await w.open('{"sub":"sam"}')
    draw(sam, el('e-1', 10))
    draw(sam, el('e-2', 20))
    await until(has(w, 'e-2'), 2000, 'the board drawn')
    assert.equal((await protect(w, ['e-1'])).status, 200)
    const record = () => w.doc().getMap(PROTECTED)
    await until(() => sam.doc.getMap(PROTECTED).has('e-1'), 2000, "the record in Sam's tab")

    sam.doc.getMap(PROTECTED).delete('e-1')
    sam.doc.getMap(PROTECTED).set('e-2', { by: 'u-sam', name: 'Sam', at: 1 })
    draw(sam, el('e-check', 1))
    await until(has(w, 'e-check'), 2000, 'a later change')
    await until(() => record().has('e-1') && !record().has('e-2'), 2000, 'the record put back')
    await w.close()
})

test('lifting gives the objects back to everyone with edit', async () => {
    const w = await world()
    const ed = await w.open('{"sub":"ed"}')
    draw(ed, el('e-1', 10))
    await until(has(w, 'e-1'), 2000, 'the board drawn')
    assert.equal((await protect(w, ['e-1'])).status, 200)
    const lifted = await protect(w, ['e-1'], false)
    assert.equal(lifted.status, 200)
    assert.equal(lifted.data.changed, 1)
    assert.equal(w.doc().getMap(PROTECTED).size, 0)
    assert.equal(at(w, 'e-1').locked, false)
    await until(() => ed.doc.getMap('elements').get('e-1')?.locked === false, 2000, "the lift in Ed's tab")

    draw(ed, { ...at(w, 'e-1'), x: 77, version: at(w, 'e-1').version + 1 })
    await until(() => at(w, 'e-1')?.x === 77, 2000, "Ed's move")
    // Lifting what isn't protected changes nothing.
    assert.equal((await protect(w, ['e-1'], false)).data.changed, 0)
    await w.close()
})

test("undoing guests' drawing leaves a protected object as it is", async () => {
    const w = await world()
    const guest = await w.open(GUEST)
    draw(guest, el('e-g', 10))
    await until(() => w.doc()?.getMap(GUEST_AUTHORS).has('e-g'), 2000, "the guest's name on their stroke")
    const kept = await w.op('guestsSnapshot', {})
    assert.equal((await protect(w, ['e-g'])).status, 200)
    const undone = await w.op('undoGuests', { state: kept.data.state })
    assert.equal(undone.status, 200)
    assert.equal(undone.data.undone, 0)
    assert.equal(at(w, 'e-g').isDeleted, false)
    assert.equal(at(w, 'e-g').locked, true)
    await w.close()
})

test('the protect op refuses what it cannot do', async () => {
    const w = await world()
    for (const [payload, why] of [
        [{ protect: true, by: SAM }, 'no objects'],
        [{ element_ids: [], protect: true, by: SAM }, 'an empty list'],
        [{ element_ids: 'e-1', protect: true, by: SAM }, 'not a list'],
        [{ element_ids: [7], protect: true, by: SAM }, 'not an id'],
        [{ element_ids: Array.from({ length: 501 }, (_, i) => `e-${i}`), protect: true, by: SAM }, 'over 500'],
        [{ element_ids: ['e-1'], by: SAM }, 'no protect flag'],
        [{ element_ids: ['e-1'], protect: true }, 'nobody behind it'],
    ]) {
        assert.equal((await w.op('protect', payload)).status, 400, why)
    }
    await w.close()
})
