import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { FakeBackend, FakeRedis, TestClient, startServer, until } from '../testkit.js'
import { BOARD_ELEMENT_TYPES, isBoardElement } from './ops/elementTypes.js'

// A board holds only the element types it draws (V-547): Excalidraw draws an
// "iframe" element as a live page made from the element's own HTML, with
// scripts, forms and popups out of its sandbox. One a crafted client wrote
// never reaches another tab, from a stored board, a socket or an op.

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

const ID = '7d1e2f3a-4b5c-4d6e-8f9a-0b1c2d3e4f5b'
const NAME = `board:${ID}`

const el = (id, x, version = 1, more = {}) => ({ id, type: 'rectangle', x, y: 0, version, versionNonce: version, isDeleted: false, ...more })
const page = (id, version = 1) => el(id, 0, version, { type: 'iframe', customData: { generationData: { status: 'done', html: '<form action="https://x.example">' } } })
// What a crafted client writes: a live page, the frame that makes one, a type no board has, and no type at all.
const crafted = (id) => [page(id), el(`${id}-m`, 0, 1, { type: 'magicframe' }), el(`${id}-u`, 0, 1, { type: 'script' }), { id: `${id}-n`, version: 1 }]

async function world(stored = []) {
    const backend = new FakeBackend()
    const doc = new Y.Doc()
    doc.transact(() => {
        for (const e of stored) doc.getMap('elements').set(e.id, e)
    })
    backend.boards.set(ID, { board_state: stored.length ? Buffer.from(Y.encodeStateAsUpdate(doc)).toString('base64') : '' })
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const tabs = []
    const open = async (sub) => {
        const tab = new TestClient({ url: collab.url, name: NAME, token: `{"sub":"${sub}"}` })
        assert.equal(await tab.connect(), 'synced')
        tabs.push(tab)
        return tab
    }
    const server = () => collab.server.hocuspocus.documents.get(NAME)
    const close = async () => {
        for (const tab of tabs) {
            await tab.close()
            tab.destroy()
        }
        await collab.stop()
        assert.deepEqual(rejections, [])
    }
    return { open, server, close }
}
const ids = (doc) => [...doc.getMap('elements').keys()].sort()
const draw = (tab, e) => tab.doc.getMap('elements').set(e.id, e)

test("the list is Excalidraw's types less the live page, the frame that makes one, and the marquee", () => {
    assert.deepEqual([...BOARD_ELEMENT_TYPES].sort(), ['arrow', 'diamond', 'ellipse', 'embeddable', 'frame', 'freedraw', 'image', 'line', 'rectangle', 'text'])
    assert.equal(isBoardElement(el('a', 0)), true)
    for (const v of [...crafted('x'), el('s', 0, 1, { type: 'selection' }), el('t', 0, 1, { type: 'toString' }), null, 'rectangle', undefined]) {
        assert.equal(isBoardElement(v), false, JSON.stringify(v) ?? String(v))
    }
})

test('a stored board opens without the elements it does not draw, for every tab', async () => {
    const w = await world([el('a', 10), ...crafted('x'), el('b', 20)])
    const sam = await w.open('sam')
    assert.deepEqual(ids(w.server()), ['a', 'b'])
    await until(() => ids(sam.doc).join() === 'a,b', 2000, "the board as Sam's tab opens it")
    await w.close()
})

test("a socket's crafted element is taken out, and one it turned into a page goes back as it was, newer", async () => {
    const w = await world([el('a', 10, 3)])
    const sam = await w.open('sam')
    const eve = await w.open('eve')
    for (const e of crafted('x')) draw(eve, e)
    draw(eve, page('a', 4))
    draw(eve, el('b', 20))
    // Sam's tab takes Eve's box, and none of the rest.
    await until(() => sam.doc.getMap('elements').has('b'), 2000, "Eve's box in Sam's tab")
    assert.deepEqual(ids(w.server()), ['a', 'b'])
    assert.deepEqual(ids(sam.doc), ['a', 'b'])
    const back = w.server().getMap('elements').get('a')
    assert.equal(back.type, 'rectangle')
    assert.ok(back.version > 4, 'the put-back is newer than the crafted copy, so its writer takes it too')
    await until(() => eve.doc.getMap('elements').get('a')?.type === 'rectangle', 2000, "the box back in Eve's tab")
    assert.equal(eve.doc.getMap('elements').has('x'), false)
    await w.close()
})

test("a change with no socket behind it (an op, a restore) is held to the list too", async () => {
    const w = await world([el('a', 10)])
    const sam = await w.open('sam')
    w.server().transact(() => {
        for (const e of crafted('y')) w.server().getMap('elements').set(e.id, e)
        w.server().getMap('elements').set('c', el('c', 30))
    }, { op: 'undoGuests' })
    await until(() => sam.doc.getMap('elements').has('c'), 2000, 'the op in Sam\'s tab')
    assert.deepEqual(ids(w.server()), ['a', 'c'])
    assert.deepEqual(ids(sam.doc), ['a', 'c'])
    await w.close()
})
