import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { createUpdateBuffer, keys } from './buffer.js'
import { FakeBackend, FakeRedis, TestClient, quietLog, startServer, until, sleep } from './testkit.js'

// Saves that cannot succeed must end. Before 12 Oct 2026:
//   - a doc deleted (or archived by the retention run) while it was open had
//     its saves refused, was quarantined, and letting go of its pinned copy
//     saved it again, which was refused again: the cycle ran every few seconds
//     until a restart, its tab was never told, and everything typed after the
//     delete was dropped;
//   - the same cycle followed any save the API kept refusing;
//   - the quarantined whole state shared a list capped at 200 entries with the
//     updates quarantined after it, so a minute of typing pushed it out;
//   - two people typing the first lines of a new doc at once: the second one's
//     first block looked like "a second copy of the doc", was dropped, and that
//     person was made read-only.

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

const ID = '9d2f6c1a-7e1b-4c55-8a5e-2f9b0c3d4e5f'
const NEW = '2b3c4d5e-6f70-4a8b-9c0d-1e2f3a4b5c6d'
const BODY = '<h1>Launch</h1><p>Ship on Thursday.</p>'
const para = (t) => {
    const p = new Y.XmlElement('paragraph')
    p.insert(0, [new Y.XmlText(t)])
    return p
}
const append = (doc, node) => doc.getXmlFragment('default').insert(doc.getXmlFragment('default').length, [node])
const text = (doc) => JSON.stringify(TiptapTransformer.fromYdoc(doc, 'default'))
const count = (collab, what) => collab.logs.lines.filter((l) => l.includes(what)).length

async function open(collab, name) {
    const tab = new TestClient({ url: collab.url, name })
    assert.equal(await tab.connect(), 'synced')
    return tab
}

test('a doc deleted while open is let go once: its tab is told, its edits are kept, nothing saves it again', async () => {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(ID, BODY)
    const collab = await startServer({ redis, backend })
    const tab = await open(collab, ID)

    // Deleted (or archived) while the tab is open: saves answer 410.
    backend.saveStatus = 410
    backend.authStatus = 410
    append(tab.doc, para('Typed after the delete.'))

    // The tab is told to open the doc again (the web app keeps its unsent
    // edits aside), and may not write any more.
    await until(() => tab.stateless.some((s) => s.includes('epoch-changed')), 3000, 'the tab to be told')
    const flag = JSON.parse(await redis.get(keys.flag(ID)))
    assert.equal(flag.source, 'gone')
    const kept = JSON.parse(await redis.get(keys.quarantineState(ID)))
    const copy = new Y.Doc()
    Y.applyUpdate(copy, Buffer.from(kept.bytes, 'base64'))
    assert.match(text(copy), /Typed after the delete\./, 'what was typed after the delete is kept')

    // Opening it again is refused as unavailable: the web app says it couldn't
    // open the doc, with Retry.
    const again = new TestClient({ url: collab.url, name: ID })
    assert.equal(await again.connect(), 'denied')
    assert.equal(again.denied, 'document-unavailable')
    await again.close()
    again.destroy()

    await tab.close()
    tab.destroy()
    await until(() => !collab.server.hocuspocus.documents.has(ID), 3000, 'the doc to unload')
    const attempts = backend.saveAttempts
    await sleep(600) // many retry periods (30, 60, 120 ms in tests)
    assert.equal(backend.saveAttempts, attempts, 'nothing tries to save it again')
    assert.equal(count(collab, 'IS GONE'), 1, 'it is let go once')
    assert.equal(collab.failures.pending(ID), false)
    assert.equal(collab.server.hocuspocus.documents.has(ID), false, 'and it stays unloaded')
    assert.equal(backend.docs.get(ID).doc_body, BODY, 'the stored copy is unchanged')
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('a save the API keeps refusing is quarantined once, not over and over', async () => {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(ID, BODY)
    backend.saveStatus = 422
    const collab = await startServer({ redis, backend })
    const tab = await open(collab, ID)
    append(tab.doc, para('Refused content.'))
    await until(async () => !!(await redis.get(keys.flag(ID))), 4000, 'the flag')
    await tab.close()
    tab.destroy()
    await until(() => !collab.server.hocuspocus.documents.has(ID), 3000, 'the doc to unload')
    const attempts = backend.saveAttempts
    await sleep(600)
    assert.equal(backend.saveAttempts, attempts, 'no save after the quarantine')
    assert.equal(count(collab, 'FLAGGED'), 1, 'flagged once')
    assert.equal(collab.failures.pending(ID), false)
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('a quarantined doc saves again once someone edits it', async () => {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(ID, BODY)
    backend.saveStatus = 422
    const collab = await startServer({ redis, backend })
    const tab = await open(collab, ID)
    append(tab.doc, para('Refused content.'))
    await until(async () => !!(await redis.get(keys.flag(ID))), 4000, 'the flag')
    backend.saveStatus = null
    append(tab.doc, para('Accepted later.'))
    await until(() => backend.docs.get(ID).doc_body.includes('Accepted later.'), 3000, 'the next save')
    await tab.close()
    tab.destroy()
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('the quarantined state outlives the updates quarantined after it', async () => {
    const redis = new FakeRedis()
    const log = quietLog()
    const buffer = createUpdateBuffer(redis, { log })
    const doc = new Y.Doc()
    append(doc, para('The whole doc.'))
    // 300 buffered updates follow (a few minutes of typing).
    for (let i = 0; i < 300; i++) await buffer.push(ID, Y.encodeStateAsUpdate(doc))
    await buffer.quarantineAll(ID, 'HTTP 410: gone', doc)
    assert.ok((await redis.llen(keys.quarantine(ID))) <= 200, 'the list keeps its cap')
    const kept = JSON.parse(await redis.get(keys.quarantineState(ID)))
    const copy = new Y.Doc()
    Y.applyUpdate(copy, Buffer.from(kept.bytes, 'base64'))
    assert.match(text(copy), /The whole doc\./)
    assert.equal(await redis.llen(keys.updates(ID)), 0, 'nothing is replayed')
})

test('a retry that cannot open a gone doc quarantines the state its failed save kept', async () => {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    const kept = new Y.Doc()
    append(kept, para('Kept before the restart.'))
    await redis.set(keys.unsaved(ID), Buffer.from(Y.encodeStateAsUpdate(kept)).toString('base64'))
    // The doc was deleted while the service was down: loading it answers 410.
    backend.getStatus = 410
    const collab = await startServer({ redis, backend })
    await collab.recoverUnsaved()
    await until(async () => !!(await redis.get(keys.flag(ID))), 4000, 'the flag')
    assert.equal(await redis.get(keys.unsaved(ID)), null, 'not retried again after the next restart')
    const state = JSON.parse(await redis.get(keys.quarantineState(ID)))
    const copy = new Y.Doc()
    Y.applyUpdate(copy, Buffer.from(state.bytes, 'base64'))
    assert.match(text(copy), /Kept before the restart\./, 'and not lost')
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('joining a doc that is gone is refused as unavailable; one refused for access is not', async () => {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(ID, BODY)
    const collab = await startServer({ redis, backend })
    for (const [status, reason] of [[410, 'document-unavailable'], [404, 'document-unavailable'], [403, 'permission-denied']]) {
        backend.authStatus = status
        const tab = new TestClient({ url: collab.url, name: ID })
        assert.equal(await tab.connect(), 'denied')
        assert.equal(tab.denied, reason, `HTTP ${status}`)
        await tab.close()
        tab.destroy()
    }
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('two people typing the first lines of a new doc at once both keep them', async () => {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(NEW, '')
    const collab = await startServer({ redis, backend })
    const a = await open(collab, NEW)
    const b = await open(collab, NEW)
    // Each types a first block before seeing the other's.
    append(a.doc, para('First line from A.'))
    append(b.doc, para('First line from B.'))
    await until(() => backend.saves.some((s) => s.id === NEW && s.html.includes('from A.') && s.html.includes('from B.')), 3000, 'a save with both')
    assert.deepEqual(a.stateless.concat(b.stateless).filter((s) => s.includes('epoch-changed')), [], 'nobody is told to reload')
    await until(() => text(a.doc).includes('from B.') && text(b.doc).includes('from A.'), 3000, 'both tabs to hold both lines')
    await a.close()
    a.destroy()
    await b.close()
    b.destroy()
    await collab.stop()
    assert.deepEqual(rejections, [])
})
