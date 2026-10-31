import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { guardRedisExtension } from './guardRedis.js'
import { SCHEMA_HASH } from './schemaGate.js'
import { storedAtOf } from './collabServer.js'
import { bodyHash, encodeState } from './docState.js'
import { FakeBackend, FakeRedis, TestClient, startServer, until, sleep } from './testkit.js'

// The collaboration server against a fake Redis and a fake API, over real
// websockets. Each case was a way to lose a doc, or every doc at once:
//   - a save that failed escaped as an unhandled rejection; index.js exited,
//     and on restart the buffered edit failed again (a crash loop);
//   - a load that failed served an empty doc, and its first edit was saved
//     over the stored one;
//   - read-only connections (guests, board viewers) could write, because the
//     flag was set where Hocuspocus 3 no longer reads it;
//   - a tab with an older editor schema deletes the blocks it does not know.

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

const ID = '9d2f6c1a-7e1b-4c55-8a5e-2f9b0c3d4e5f'
const OTHER = '1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d'
// Stored as a doc is once its blocks have ids (transforms/blockIds.js gives a
// doc without them its ids as it loads: transforms/blockIds.test.js).
const BODY = '<h1 data-block-id="b-launch">Launch</h1><p data-block-id="b-ship">Ship on Thursday.</p>'
const text = (doc) => JSON.stringify(TiptapTransformer.fromYdoc(doc, 'default'))
const para = (t, type = 'paragraph') => {
    const p = new Y.XmlElement(type)
    p.insert(0, [new Y.XmlText(t)])
    return p
}
const append = (doc, node) => doc.getXmlFragment('default').insert(doc.getXmlFragment('default').length, [node])

// The world a restart keeps: Redis and the API. The server process is new.
const world = () => {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(ID, BODY)
    backend.doc(OTHER, '<p data-block-id="b-another">Another doc.</p>')
    return { redis, backend }
}

async function edit(collab, name, mutate) {
    const tab = new TestClient({ url: collab.url, name })
    assert.equal(await tab.connect(), 'synced')
    mutate(tab.doc)
    return tab
}

test('an unknown block is saved, kept, and survives a restart and a replay', async () => {
    const { redis, backend } = world()
    let collab = await startServer({ redis, backend })
    const tab = await edit(collab, ID, (doc) => {
        append(doc, new Y.XmlElement('futureEmbed'))
        append(doc, para('After the embed.'))
    })
    await until(() => backend.docs.get(ID).doc_body.includes('After the embed.'), 3000, 'the save')
    assert.match(backend.docs.get(ID).doc_body, /data-oc-preserved="futureEmbed"/)
    await tab.close()
    tab.destroy()
    await collab.stop()

    // Restart: the doc opens again with the block, and saves again.
    collab = await startServer({ redis, backend })
    const again = await edit(collab, ID, (doc) => append(doc, para('After the restart.')))
    assert.ok(again.doc.getXmlFragment('default').toArray().some((n) => n.nodeName === 'futureEmbed'))
    await until(() => backend.docs.get(ID).doc_body.includes('After the restart.'), 3000, 'the second save')
    assert.match(backend.docs.get(ID).doc_body, /data-oc-preserved="futureEmbed"/)
    await again.close()
    again.destroy()
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('a saved state from before toggles had a first line opens moved over, the new line given an id (B2)', async () => {
    const { redis, backend } = world()
    const old = new Y.Doc()
    old.transact(() => {
        const toggle = new Y.XmlElement('collapsible')
        toggle.setAttribute('title', 'Risks')
        toggle.setAttribute('open', false)
        toggle.setAttribute('blockId', 'b-risks')
        const inside = para('Body')
        inside.setAttribute('blockId', 'b-body')
        toggle.insert(0, [inside])
        old.getXmlFragment('default').insert(0, [toggle])
    })
    const body = '<div data-type="collapsible" data-title="Risks" data-open="false" data-block-id="b-risks"><p data-block-id="b-body">Body</p></div>'
    backend.doc(ID, body, { doc_yjs_state: encodeState(old), doc_yjs_body_hash: bodyHash(body) })
    const collab = await startServer({ redis, backend })
    const tab = await edit(collab, ID, () => {})
    const toggle = TiptapTransformer.fromYdoc(tab.doc, 'default').content[0]
    assert.equal(toggle.type, 'collapsible')
    assert.equal(toggle.attrs.title, undefined)
    assert.equal(toggle.attrs.open, undefined)
    assert.deepEqual(toggle.content.map((n) => n.content?.[0]?.text), ['Risks', 'Body'])
    assert.ok(toggle.content[0].attrs.blockId, 'the new first line has an id')
    assert.equal(toggle.content[1].attrs.blockId, 'b-body')
    await until(() => !backend.docs.get(ID).doc_body.includes('data-title'), 3000, 'the save of the moved-over doc')
    await tab.close()
    tab.destroy()
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('a backend failure keeps the edit, survives a restart, and saves once the backend is back', async () => {
    const { redis, backend } = world()
    backend.saveStatus = 503
    let collab = await startServer({ redis, backend })
    const tab = await edit(collab, ID, (doc) => append(doc, para('Typed while the API was down.')))
    await until(() => collab.logs.lines.some((l) => l.includes('SAVE FAILED') && l.includes(ID)), 3000, 'the failure log')
    assert.ok(await redis.get(`collab:unsaved:${ID}`), 'the whole state is kept')
    assert.ok((await redis.llen(`collab:updates:${ID}`)) > 0, 'the updates are kept')
    // Other docs still open while this one fails.
    const other = new TestClient({ url: collab.url, name: OTHER })
    assert.equal(await other.connect(), 'synced')
    await other.close()
    other.destroy()
    await tab.close()
    tab.destroy()
    await collab.stop()
    assert.doesNotMatch(backend.docs.get(ID).doc_body, /Typed while/)

    // Restart while the API is still down: the kept state is retried, fails
    // again, nothing crashes.
    collab = await startServer({ redis, backend })
    await collab.recoverUnsaved()
    await until(() => collab.logs.lines.filter((l) => l.includes('SAVE FAILED')).length >= 1, 3000, 'a retry')
    // The API comes back: the next retry saves it, once.
    backend.saveStatus = null
    await until(() => backend.docs.get(ID).doc_body.includes('Typed while the API was down.'), 5000, 'the recovered save')
    assert.equal(backend.docs.get(ID).doc_body.split('Typed while the API was down.').length - 1, 1)
    await until(async () => !(await redis.get(`collab:unsaved:${ID}`)), 3000, 'the kept state to clear')
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('a Redis lock error does not stop the save, before or after a restart', async () => {
    const { redis, backend } = world()
    // As the Hocuspocus Redis extension does when Redis is down.
    const lockThatFails = () => guardRedisExtension({
        priority: 1000,
        async onStoreDocument() { throw new Error('Connection is closed.') },
        async afterStoreDocument() {},
        async onChange() { throw new Error('Connection is closed.') },
    })
    let collab = await startServer({ redis, backend, serverExtensions: [lockThatFails()] })
    const tab = await edit(collab, ID, (doc) => append(doc, para('Saved without the lock.')))
    await until(() => backend.docs.get(ID).doc_body.includes('Saved without the lock.'), 3000, 'the save')
    await tab.close()
    tab.destroy()
    await collab.stop()

    collab = await startServer({ redis, backend, serverExtensions: [lockThatFails()] })
    const again = await edit(collab, ID, (doc) => append(doc, para('And after the restart.')))
    await until(() => backend.docs.get(ID).doc_body.includes('And after the restart.'), 3000, 'the second save')
    await again.close()
    again.destroy()
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('another instance holding the lock still skips the save quietly', async () => {
    const { redis, backend } = world()
    const busy = guardRedisExtension({
        priority: 1000,
        async onStoreDocument() { throw new Error('', { cause: 'locked by another instance' }) },
        async afterStoreDocument() {},
    })
    const collab = await startServer({ redis, backend, serverExtensions: [busy] })
    const tab = await edit(collab, ID, (doc) => append(doc, para('Saved by the other instance.')))
    await sleep(150)
    assert.equal(backend.saves.filter((s) => s.html).length, 0)
    await tab.close()
    tab.destroy()
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('a save the API refuses is quarantined after three tries; the stored copy stays', async () => {
    const { redis, backend } = world()
    backend.saveStatus = 422
    const collab = await startServer({ redis, backend })
    const tab = await edit(collab, ID, (doc) => append(doc, para('Refused content.')))
    await until(async () => !!(await redis.get(`collab:flag:${ID}`)), 4000, 'the flag')
    const flag = JSON.parse(await redis.get(`collab:flag:${ID}`))
    assert.equal(flag.source, 'save')
    assert.ok((await redis.llen(`collab:quarantine:${ID}`)) > 0, 'its bytes are kept')
    assert.equal(await redis.llen(`collab:updates:${ID}`), 0, 'and never replayed')
    assert.equal(await redis.get(`collab:unsaved:${ID}`), null)
    assert.equal(backend.docs.get(ID).doc_body, BODY)
    await tab.close()
    tab.destroy()
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('a buffered update that cannot be applied is quarantined and the doc opens', async () => {
    const { redis, backend } = world()
    await redis.rpush(`collab:updates:${ID}`, Buffer.from('not a yjs update').toString('base64'))
    const collab = await startServer({ redis, backend })
    const tab = new TestClient({ url: collab.url, name: ID })
    assert.equal(await tab.connect(), 'synced')
    assert.match(text(tab.doc), /Ship on Thursday/)
    assert.equal(await redis.llen(`collab:updates:${ID}`), 0)
    assert.equal(await redis.llen(`collab:quarantine:${ID}`), 1)
    assert.equal(JSON.parse(await redis.get(`collab:flag:${ID}`)).source, 'replay')
    await tab.close()
    tab.destroy()
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('a doc that could not be read is refused, never opened empty', async () => {
    const { redis, backend } = world()
    backend.getStatus = 503
    const collab = await startServer({ redis, backend })
    const tab = new TestClient({ url: collab.url, name: ID })
    assert.equal(await tab.connect(), 'denied')
    assert.equal(tab.denied, 'document-unavailable', 'the web app shows "Couldn\'t open this doc" with Retry')
    assert.equal(tab.synced, false, 'no empty doc was served')
    assert.equal(collab.server.hocuspocus.documents.has(ID), false)
    // An edit made anyway goes nowhere, and the stored body is untouched.
    append(tab.doc, para('Typed into nothing.'))
    await sleep(100)
    assert.equal(backend.docs.get(ID).doc_body, BODY)
    await tab.close()
    tab.destroy()

    // Back again: the doc opens with its content.
    backend.getStatus = null
    const retry = new TestClient({ url: collab.url, name: ID })
    assert.equal(await retry.connect(), 'synced')
    assert.match(text(retry.doc), /Ship on Thursday/)
    await retry.close()
    retry.destroy()
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('a board that could not be read is refused too', async () => {
    const { redis, backend } = world()
    backend.getStatus = 503
    const collab = await startServer({ redis, backend })
    const tab = new TestClient({ url: collab.url, name: `board:${ID}` })
    assert.equal(await tab.connect(), 'denied')
    assert.equal(tab.denied, 'document-unavailable')
    await tab.close()
    tab.destroy()
    await collab.stop()
})

test("a guest's connection is read-only: its deletions never reach the doc", async () => {
    const { redis, backend } = world()
    const collab = await startServer({ redis, backend })
    const guest = new TestClient({ url: collab.url, name: ID, token: '{"guest":true,"sub":"guest-1"}' })
    assert.equal(await guest.connect(), 'synced')
    assert.equal(guest.scope, 'readonly')
    // What a guest editor lacking a block type does: deletes it from its copy.
    guest.doc.getXmlFragment('default').delete(0, guest.doc.getXmlFragment('default').length)
    await until(() => guest.statuses.includes(false), 2000, 'the refusal')
    const live = collab.server.hocuspocus.documents.get(ID)
    assert.match(text(live), /Ship on Thursday/)
    await guest.close()
    guest.destroy()
    await collab.stop()
    assert.equal(backend.docs.get(ID).doc_body, BODY)
    assert.deepEqual(rejections, [])
})

test("a call's guest draws on the board on its stage only when the API says so", async () => {
    const { redis, backend } = world()
    backend.boards.set(ID, { board_state: '' })
    const collab = await startServer({ redis, backend })
    const token = '{"guest":true,"sub":"guest-g-1","name":"Guest: Jane","call":"s-1"}'

    // "Guests can draw" off (the default): the API answers canEdit false.
    backend.levels.set('guest-g-1', 'view')
    const reader = new TestClient({ url: collab.url, name: `board:${ID}`, token })
    assert.equal(await reader.connect(), 'synced')
    assert.equal(reader.scope, 'readonly')
    reader.doc.getMap('elements').set('e-1', { id: 'e-1', version: 1 })
    await until(() => reader.statuses.includes(false), 2000, 'the refusal')
    assert.equal(collab.server.hocuspocus.documents.get(`board:${ID}`).getMap('elements').has('e-1'), false)
    await reader.close()
    reader.destroy()

    // On: the reconnect (a reauth closes the old socket) draws.
    backend.levels.set('guest-g-1', 'edit')
    const drawer = new TestClient({ url: collab.url, name: `board:${ID}`, token })
    assert.equal(await drawer.connect(), 'synced')
    assert.equal(drawer.scope, 'read-write')
    drawer.doc.getMap('elements').set('e-2', { id: 'e-2', version: 1 })
    await until(() => collab.server.hocuspocus.documents.get(`board:${ID}`).getMap('elements').has('e-2'), 2000, 'the stroke')
    await drawer.close()
    drawer.destroy()
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('an editor a hard lock holds at comment reconnects read-only: their edits never reach the doc', async () => {
    const { redis, backend } = world()
    const collab = await startServer({ redis, backend })
    const token = '{"sub":"editor-1"}'

    const before = new TestClient({ url: collab.url, name: ID, token })
    assert.equal(await before.connect(), 'synced')
    assert.equal(before.scope, 'read-write')
    await before.close()
    before.destroy()

    // The owner locks the doc hard: the session answers this editor at
    // comment (the Access Resolver holds them there), and their next
    // connection (a reauth closes the old one) is read-only.
    backend.levels.set('editor-1', 'comment')
    const after = new TestClient({ url: collab.url, name: ID, token })
    assert.equal(await after.connect(), 'synced')
    assert.equal(after.scope, 'readonly')
    after.doc.getXmlFragment('default').delete(0, after.doc.getXmlFragment('default').length)
    await until(() => after.statuses.includes(false), 2000, 'the refusal')
    assert.match(text(collab.server.hocuspocus.documents.get(ID)), /Ship on Thursday/)
    await after.close()
    after.destroy()
    await collab.stop()
    assert.equal(backend.docs.get(ID).doc_body, BODY)
    assert.deepEqual(rejections, [])
})

test('a tab built against an older editor schema reads but cannot write, and is told to reload', async () => {
    const { redis, backend } = world()
    const schema = { current: SCHEMA_HASH, history: ['an-older-manifest', SCHEMA_HASH] }
    const collab = await startServer({ redis, backend, schema })

    const stale = new TestClient({ url: collab.url, name: ID, schema: 'an-older-manifest' })
    assert.equal(await stale.connect(), 'synced')
    assert.equal(stale.scope, 'readonly')
    const told = () => stale.stateless.map((m) => JSON.parse(m))
    await until(() => told().some((m) => m.type === 'schema-outdated'), 2000, 'the reload message')
    // Its session says so too: it may not write.
    assert.equal(told().find((m) => m.type === 'session')?.canEdit, false)
    stale.doc.getXmlFragment('default').delete(0, 1)
    await sleep(100)
    assert.match(text(collab.server.hocuspocus.documents.get(ID)), /Launch/)
    await stale.close()
    stale.destroy()

    for (const hash of [SCHEMA_HASH, undefined, 'a-newer-manifest']) {
        const tab = new TestClient({ url: collab.url, name: ID, schema: hash })
        assert.equal(await tab.connect(), 'synced')
        assert.equal(tab.scope, 'read-write', `schema ${hash}`)
        await tab.close()
        tab.destroy()
    }
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('a tab opening a stored doc hears when it was stored, not when it was read', () => {
    assert.equal(storedAtOf({ doc_updated_at: '2026-10-01T08:00:00Z' }), Date.parse('2026-10-01T08:00:00Z'))
    for (const doc of [{ doc_updated_at: '0001-01-01T00:00:00Z' }, {}, null]) {
        const before = Date.now()
        assert.ok(storedAtOf(doc) >= before, JSON.stringify(doc))
    }
})

// Hocuspocus 3.4.3 closed such a connection twice: Document.removeConnection
// sends the presence removal to the leaving connection too, and its failed send
// closes it again. Every tab with a person on it leaves this way.
test('a tab that leaves with its presence set is reported gone once, and the others hear it leave', async () => {
    const backend = new FakeBackend()
    backend.doc('d1', '<p>hello</p>')
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const ann = new TestClient({ url: collab.url, name: 'd1', token: '{"sub":"ann"}' })
    const sam = new TestClient({ url: collab.url, name: 'd1', token: '{"sub":"sam"}' })
    assert.equal(await ann.connect(), 'synced')
    assert.equal(await sam.connect(), 'synced')
    ann.presence({ user: { name: 'Ann' } })
    sam.presence({ user: { name: 'Sam' } })
    await until(() => sam.others.has(ann.doc.clientID), 3000, 'Sam sees Ann')
    const gone = () => collab.logs.lines.filter((l) => l.includes('Client disconnected from d1'))

    await ann.close()
    await until(() => !sam.others.has(ann.doc.clientID), 3000, 'Sam hears Ann leave')
    await sam.close()
    await until(() => collab.logs.lines.some((l) => l.includes('Document d1 unloaded')), 3000, 'd1 unloaded')

    assert.deepEqual(gone().map((l) => l.replace(/^.*\(/, '(')), ['(remaining: 1)', '(remaining: 0)'])
})

test('a server-side connection opened without a context is reported gone once and lets its doc go', async () => {
    const backend = new FakeBackend()
    backend.doc('d2', '<p>hello</p>')
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const connection = await collab.server.hocuspocus.openDirectConnection('d2')
    await connection.disconnect()
    await until(() => collab.logs.lines.some((l) => l.includes('Document d2 unloaded')), 3000, 'd2 unloaded')
    assert.deepEqual(collab.logs.lines.filter((l) => l.includes('Client disconnected from d2')), ['[Collab] Client disconnected from d2 (remaining: 0)'])
})

// 23 Oct 2026: a tab on the demo signed in and never got its doc, and the
// server's lines couldn't say whether the tab's first sync had reached it.
test("each connection's first sync is said once, with the connection, the doc and how long after it connected", async () => {
    const backend = new FakeBackend()
    backend.doc('d3', '<p>hello</p>')
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const ann = new TestClient({ url: collab.url, name: 'd3', token: '{"sub":"ann"}' })
    assert.equal(await ann.connect(), 'synced')
    // Asked again on the same connection (a provider's resync): answered, not
    // said again. The second sync status is the server's answer to it.
    ann.send(0, 0, Y.encodeStateVector(ann.doc))
    await until(() => ann.statuses.length >= 2, 3000, 'the second sync answered')
    const sam = new TestClient({ url: collab.url, name: 'd3', token: '{"sub":"sam"}' })
    assert.equal(await sam.connect(), 'synced')

    const said = collab.logs.lines.filter((l) => l.includes('first sync'))
    assert.equal(said.length, 2, said.join('\n'))
    const ids = said.map((l) => l.match(/^\[Collab\] d3: answered the first sync of connection ([0-9a-f-]{36}), \d+ ms after it connected$/)?.[1])
    assert.ok(ids.every(Boolean), said.join('\n'))
    assert.notEqual(ids[0], ids[1])
    // Each names the connection its connecting line named.
    for (const id of ids) assert.ok(collab.logs.lines.includes(`[Collab] Client connecting to d3 (connection ${id})`), id)
})
