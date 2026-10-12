import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { generateJSON } from '@tiptap/html'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { bringsSecondCopy, epochIn, epochOf, rebuildFromBody, syncUpdateOf } from './lineage.js'
import { FakeBackend, FakeRedis, TestClient, startServer, until, sleep } from './testkit.js'

// An open doc must hold one copy of itself, however often the server rebuilds
// it from HTML and however often a tab reconnects (11 Oct 2026: the demo's
// "Q4 launch plan" went from 7 blocks to 77 in six minutes, one full copy per
// reconnect, because every rebuild was a new Yjs history).

const BODY = '<h1>Q4 launch plan</h1><p>We launch on the last Tuesday.</p><ul><li><p>Ship the beta</p></li></ul><p>Owners: Sam, Priya.</p>'
const ID = '4b0c4b1e-3f63-4a3a-9f0e-6b1f0a7c2d11'
const times = (doc, text) => JSON.stringify(TiptapTransformer.fromYdoc(doc, 'default')).split(text).length - 1
const blocks = (doc) => doc.getXmlFragment('default').length

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

test('two rebuilds of the same body are byte-identical', () => {
    const a = rebuildFromBody(ID, BODY, COLLAB_EXTENSIONS)
    const b = rebuildFromBody(ID, BODY, COLLAB_EXTENSIONS)
    assert.deepEqual(Buffer.from(Y.encodeStateAsUpdate(a.ydoc)), Buffer.from(Y.encodeStateAsUpdate(b.ydoc)))
    assert.equal(epochIn(a.ydoc), epochOf(ID, BODY))
    // ...so merging them changes nothing.
    const merged = new Y.Doc()
    Y.applyUpdate(merged, Y.encodeStateAsUpdate(a.ydoc))
    Y.applyUpdate(merged, Y.encodeStateAsUpdate(b.ydoc))
    assert.equal(blocks(merged), 4)
    assert.equal(times(merged, 'We launch on the last Tuesday'), 1)
})

test('a different body is a different epoch', () => {
    assert.notEqual(epochOf(ID, BODY), epochOf(ID, BODY + '<p>More.</p>'))
    assert.notEqual(epochOf(ID, BODY), epochOf('another-doc', BODY))
})

test('an update carrying a whole second copy is recognised; ordinary edits are not', () => {
    const server = rebuildFromBody(ID, BODY, COLLAB_EXTENSIONS).ydoc
    // An earlier lineage: the same text under other ids (the old random rebuild).
    const old = TiptapTransformer.toYdoc(generateJSON(BODY, COLLAB_EXTENSIONS), 'default', COLLAB_EXTENSIONS)
    assert.equal(bringsSecondCopy(Y.encodeStateAsUpdate(old), server), true)
    // The same lineage again (a tab that holds the deterministic rebuild).
    assert.equal(bringsSecondCopy(Y.encodeStateAsUpdate(rebuildFromBody(ID, BODY, COLLAB_EXTENSIONS).ydoc), server), false)
    // Edits at the start, the end and offline, on the same lineage.
    const tab = new Y.Doc()
    Y.applyUpdate(tab, Y.encodeStateAsUpdate(server))
    const updates = []
    tab.on('update', (u) => updates.push(u))
    const para = (t) => { const p = new Y.XmlElement('paragraph'); p.insert(0, [new Y.XmlText(t)]); return p }
    tab.getXmlFragment('default').insert(0, [para('First')])
    tab.getXmlFragment('default').insert(tab.getXmlFragment('default').length, [para('Last')])
    for (const u of updates) assert.equal(bringsSecondCopy(u, server), false)
    // Into an empty doc anything goes (a new doc's first words).
    assert.equal(bringsSecondCopy(Y.encodeStateAsUpdate(old), new Y.Doc()), false)
})

test('the update inside a Hocuspocus sync message is found', () => {
    const u = Y.encodeStateAsUpdate(rebuildFromBody(ID, BODY, COLLAB_EXTENSIONS).ydoc)
    const frame = (type, sub, payload) => {
        const name = Buffer.from(ID)
        const len = (n) => { const o = []; while (n > 0x7f) { o.push((n & 0x7f) | 0x80); n = Math.floor(n / 128) } o.push(n); return o }
        return Buffer.from([...len(name.length), ...name, ...len(type), ...len(sub), ...len(payload.length), ...payload])
    }
    assert.deepEqual(Buffer.from(syncUpdateOf(frame(0, 2, u))), Buffer.from(u))
    assert.deepEqual(Buffer.from(syncUpdateOf(frame(0, 1, u))), Buffer.from(u))
    assert.equal(syncUpdateOf(frame(0, 0, u)), null, 'sync step 1 carries no content')
    assert.equal(syncUpdateOf(frame(1, 0, u)), null, 'awareness')
    assert.equal(syncUpdateOf(Buffer.from([200])), null, 'garbage')
})

// The probe's scenario, end to end over a websocket: a tab opens the doc, the
// server unloads it when the tab drops, rebuilds it from the body on the next
// open, and the tab reconnects with its copy, again and again.
test('rebuild, reconnect, reconnect: one copy, in the tab and on the server', async () => {
    for (const persisted of [true, false]) {
        const redis = new FakeRedis()
        const backend = new FakeBackend()
        backend.doc(ID, BODY)
        // Without a saved state every open is a rebuild: the case on the demo
        // after `demoseed --refresh`.
        backend.dropStateOnly = !persisted
        const collab = await startServer({ redis, backend })
        const tab = new TestClient({ url: collab.url, name: ID })
        for (let round = 0; round < 4; round++) {
            assert.equal(await tab.connect(), 'synced', `round ${round}`)
            await sleep(30)
            assert.equal(blocks(tab.doc), 4, `tab, round ${round}`)
            assert.equal(times(tab.doc, 'We launch on the last Tuesday'), 1)
            const live = collab.server.hocuspocus.documents.get(ID)
            assert.equal(times(live, 'We launch on the last Tuesday'), 1, `server, round ${round}`)
            await tab.close()
            await until(() => !collab.server.hocuspocus.documents.has(ID), 3000, 'the doc to unload')
        }
        if (persisted) assert.ok(backend.docs.get(ID).doc_yjs_state, 'the rebuilt state was saved')
        assert.equal(backend.docs.get(ID).doc_body, BODY, 'nothing was saved over the body')
        tab.destroy()
        await collab.stop()
    }
    assert.deepEqual(rejections, [])
})

test('a tab holding an earlier copy cannot add it: its edits are dropped and it is told to reload', async () => {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(ID, BODY)
    const collab = await startServer({ redis, backend })
    const tab = new TestClient({ url: collab.url, name: ID })
    assert.equal(await tab.connect(), 'synced')
    await tab.close()
    await until(() => !collab.server.hocuspocus.documents.has(ID), 3000, 'unload')

    // The body changes underneath (a restore, an API write, demoseed).
    const restored = '<h1>Q4 launch plan</h1><p>Restored: we launch in March.</p>'
    backend.doc(ID, restored)

    assert.equal(await tab.connect(), 'synced')
    await until(() => tab.stateless.length > 0, 2000, 'the reload message')
    assert.deepEqual(tab.stateless.map((m) => JSON.parse(m).type), ['epoch-changed'])
    const live = collab.server.hocuspocus.documents.get(ID)
    assert.equal(times(live, 'We launch on the last Tuesday'), 0, 'the old copy never reached the server')
    assert.equal(times(live, 'Restored: we launch in March.'), 1)
    // Its later edits are dropped too (read-only from then on).
    const p = new Y.XmlElement('paragraph')
    p.insert(0, [new Y.XmlText('typed after')])
    tab.doc.getXmlFragment('default').insert(0, [p])
    await sleep(100)
    assert.equal(times(live, 'typed after'), 0)
    await tab.close()
    tab.destroy()
    await collab.stop()
    assert.equal(backend.docs.get(ID).doc_body, restored)
    assert.deepEqual(rejections, [])
})

test('a buffered merge of an earlier copy is quarantined, not replayed', async () => {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(ID, BODY)
    const old = TiptapTransformer.toYdoc(generateJSON(BODY, COLLAB_EXTENSIONS), 'default', COLLAB_EXTENSIONS)
    await redis.rpush(`collab:updates:${ID}`, Buffer.from(Y.encodeStateAsUpdate(old)).toString('base64'))
    const collab = await startServer({ redis, backend })
    const tab = new TestClient({ url: collab.url, name: ID })
    assert.equal(await tab.connect(), 'synced')
    assert.equal(times(tab.doc, 'We launch on the last Tuesday'), 1)
    assert.equal((await redis.lrange(`collab:updates:${ID}`, 0, -1)).length, 0)
    assert.equal((await redis.lrange(`collab:quarantine:${ID}`, 0, -1)).length, 1)
    assert.ok(await redis.get(`collab:flag:${ID}`))
    await tab.close()
    tab.destroy()
    await collab.stop()
    assert.deepEqual(rejections, [])
})
