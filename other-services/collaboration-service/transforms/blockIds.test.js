import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { getSchema } from '@tiptap/core'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { COLLAB_EXTENSIONS } from '../extensions.js'
import { BLOCK_ID_ATTR } from '../schema/blockId.js'
import { FIELD, epochIn, rebuildFromBody } from '../lineage.js'
import { assignBlockIds, blockTypesIn, blocksNeedingIds } from './blockIds.js'
import { FakeBackend, FakeRedis, TestClient, startServer, until } from '../testkit.js'

// Block ids for docs written before blocks had them (transforms/blockIds.js):
// given once, the same however often or wherever it runs, and nothing but the
// ids ever changes (no block is added, moved or doubled).

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

const TYPES = blockTypesIn(getSchema(COLLAB_EXTENSIONS))
const ID = '6f1d2c3b-4a5e-4f60-8b7a-9c0d1e2f3a4b'
const BODY = '<h1>Launch</h1><p>Ship on <strong>Thursday</strong>, says <span data-type="mention" data-id="u1" data-label="Sam">@Sam</span>.</p>' +
    '<ul><li><p>one</p></li><li><p>two</p></li></ul><div data-type="callout"><p>inside</p></div><div data-type="poll" data-id="p1"></div>'

// [nodeName, blockId] for every element, in document order.
function elements(ydoc) {
    const out = []
    const walk = (parent) => {
        for (const child of parent.toArray()) {
            if (!(child instanceof Y.XmlElement)) continue
            out.push([child.nodeName, child.getAttribute(BLOCK_ID_ATTR) ?? null])
            walk(child)
        }
    }
    walk(ydoc.getXmlFragment(FIELD))
    return out
}
// The doc's content with every block id left out (and an attrs left empty by it).
const withoutIds = (v) => {
    if (Array.isArray(v)) return v.map(withoutIds)
    if (!v || typeof v !== 'object') return v
    const out = {}
    for (const [k, x] of Object.entries(v)) {
        if (k === BLOCK_ID_ATTR) continue
        const y = withoutIds(x)
        if (k === 'attrs' && Object.keys(y).length === 0) continue
        out[k] = y
    }
    return out
}
const content = (ydoc) => JSON.stringify(withoutIds(TiptapTransformer.fromYdoc(ydoc, FIELD)))
const legacy = () => rebuildFromBody(ID, BODY, COLLAB_EXTENSIONS).ydoc
const copyOf = (ydoc) => {
    const d = new Y.Doc()
    Y.applyUpdate(d, Y.encodeStateAsUpdate(ydoc))
    return d
}

test('the blocks are the nodes that are not inline; the doc, text and chips are not', () => {
    for (const name of ['paragraph', 'heading', 'bulletList', 'listItem', 'callout', 'poll', 'image', 'taskItem', 'codeBlock', 'horizontalRule']) assert.ok(TYPES.has(name), name)
    for (const name of ['doc', 'text', 'hardBreak', 'mention', 'channelMention', 'entityMention']) assert.ok(!TYPES.has(name), name)
})

test('every block gets an id of its own, and nothing else changes', () => {
    const ydoc = legacy()
    const before = content(ydoc)
    const blocks = elements(ydoc).filter(([n]) => TYPES.has(n)).length
    assert.equal(assignBlockIds(ydoc, { docName: ID, types: TYPES }), blocks)
    const ids = elements(ydoc).filter(([n]) => TYPES.has(n)).map(([, id]) => id)
    assert.ok(ids.every((id) => /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(id)), 'UUIDs, as the browser makes')
    assert.equal(new Set(ids).size, ids.length)
    assert.ok(elements(ydoc).filter(([n]) => !TYPES.has(n)).every(([, id]) => id === null), 'an inline chip has none')
    assert.equal(content(ydoc), before)
})

test('a second run gives nothing and writes nothing', () => {
    const ydoc = legacy()
    assignBlockIds(ydoc, { docName: ID, types: TYPES })
    const sv = Y.encodeStateVector(ydoc)
    let updates = 0
    ydoc.on('update', () => updates++)
    assert.equal(assignBlockIds(ydoc, { docName: ID, types: TYPES }), 0)
    assert.equal(updates, 0)
    assert.deepEqual(Y.encodeStateVector(ydoc), sv)
})

test('two servers giving one doc its ids at once agree, and merging them doubles nothing', () => {
    const base = legacy()
    const a = copyOf(base)
    const b = copyOf(base)
    assignBlockIds(a, { docName: ID, types: TYPES })
    assignBlockIds(b, { docName: ID, types: TYPES })
    assert.deepEqual(elements(a), elements(b), 'the same ids')
    Y.applyUpdate(a, Y.encodeStateAsUpdate(b))
    Y.applyUpdate(b, Y.encodeStateAsUpdate(a))
    assert.deepEqual(elements(a), elements(b))
    assert.deepEqual(elements(a), elements(copyOf(b)))
    assert.equal(content(a), content(base), 'one copy of every block')
    assert.equal(elements(a).length, elements(base).length)
})

test('a block keeps the id it has; a copy of another block gets its own', () => {
    const ydoc = new Y.Doc()
    const para = (text, id) => {
        const p = new Y.XmlElement('paragraph')
        if (id) p.setAttribute(BLOCK_ID_ATTR, id)
        p.insert(0, [new Y.XmlText(text)])
        return p
    }
    ydoc.getXmlFragment(FIELD).insert(0, [para('first', 'same'), para('pasted copy', 'same'), para('none')])
    assert.deepEqual(blocksNeedingIds(ydoc.getXmlFragment(FIELD), TYPES).length, 2)
    assert.equal(assignBlockIds(ydoc, { docName: ID, types: TYPES }), 2)
    const ids = elements(ydoc).map(([, id]) => id)
    assert.equal(ids[0], 'same', 'the first keeps its id')
    assert.notEqual(ids[1], 'same')
    assert.equal(new Set(ids).size, 3)
})

test('a doc written before block ids gets them as it loads, once, and keeps them', async () => {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(ID, BODY)
    const collab = await startServer({ redis, backend })
    const tab = new TestClient({ url: collab.url, name: ID })
    try {
        assert.equal(await tab.connect(), 'synced')
        await until(() => elements(tab.doc).filter(([n]) => TYPES.has(n)).every(([, id]) => id), 3000, 'the ids in the tab')
        const given = elements(tab.doc)
        const epoch = epochIn(tab.doc)
        await until(() => (backend.docs.get(ID).doc_body.match(/data-block-id=/g) || []).length === given.filter(([n]) => TYPES.has(n)).length, 3000, 'the ids in the stored body')
        assert.ok(backend.docs.get(ID).doc_yjs_state, 'with the state they were given in')

        await tab.close()
        await until(() => !collab.server.hocuspocus.documents.has(ID), 3000, 'the doc to unload')
        // Opened again: the stored state, the same ids, one copy, the same lineage.
        assert.equal(await tab.connect(), 'synced')
        const live = collab.server.hocuspocus.documents.get(ID)
        assert.deepEqual(elements(live), given)
        assert.deepEqual(elements(tab.doc), given)
        assert.equal(epochIn(live), epoch)
        // Each connection is told its session (session.js) and when its doc is
        // stored (persisted, A4); never to reload.
        const told = tab.stateless.map((m) => JSON.parse(m).type).filter((t) => t !== 'session' && t !== 'persisted')
        assert.deepEqual(told, [], 'never told to reload')
        await tab.close()
    } finally {
        tab.destroy()
        await collab.stop()
    }
    assert.deepEqual(rejections, [])
})
