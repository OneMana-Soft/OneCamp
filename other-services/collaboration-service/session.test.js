// The Doc Session on the server (session.js): who joins a doc live and how far,
// and what each tab is told. Real Hocuspocus over real websockets, with the
// testkit's fake API (each person's level by their token's sub).

import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
import { test } from 'node:test'
import * as Y from 'yjs'
import { FakeBackend, FakeRedis, TestClient, sleep, startServer, until } from './testkit.js'
import { persistedMessage } from './persisted.js'
import { REFUSED, accessFrom, authorizeDoc, reasonFor, sessionMessage } from './session.js'
import { SCHEMA_HASH } from './schemaGate.js'

const require = createRequire(import.meta.url)
const WebSocket = createRequire(require.resolve('@hocuspocus/server'))('ws')

const ID = 'doc-session-1'
const BODY = '<h1>Q4 launch plan</h1><p>We launch on the last Tuesday.</p>'
const token = (sub) => JSON.stringify({ sub })
const told = (client) => client.stateless.map((m) => JSON.parse(m))
const sessionOf = (client) => told(client).filter((m) => m.type === 'session').at(-1)
const text = (doc) => doc.getXmlFragment('default').toString()
const count = (haystack, needle) => haystack.split(needle).length - 1

const rejections = []
process.on('unhandledRejection', (r) => rejections.push(r))

function world() {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(ID, BODY)
    return { redis, backend }
}

// ---------------------------------------------------------------------------
// The API's answer
// ---------------------------------------------------------------------------

test('the API answer becomes the session, and a failure names its reason', async () => {
    assert.deepEqual(accessFrom({ level: 'comment', canEdit: false }), { level: 'comment', canEdit: false, locked: false })
    assert.deepEqual(accessFrom({ level: 'weird', canEdit: 'yes' }), { level: 'view', canEdit: false, locked: false })
    assert.equal(reasonFor(401), REFUSED.unauthenticated)
    assert.equal(reasonFor(403), REFUSED.forbidden)
    assert.equal(reasonFor(404), REFUSED.notFound)
    assert.equal(reasonFor(410), REFUSED.gone)
    assert.equal(reasonFor(503), REFUSED.unavailable)
    assert.equal(reasonFor(undefined), REFUSED.unavailable)
})

test('the session is asked with the person\'s token and the internal secret', async () => {
    const backend = new FakeBackend()
    backend.levels.set('reader', 'view')
    const access = await authorizeDoc({ api: backend.api, backendUrl: 'http://api.test', token: token('reader'), id: ID, secret: 's' })
    assert.deepEqual(access, { level: 'view', canEdit: false, locked: false })
    assert.deepEqual(backend.sessionAsks, [{ sub: 'reader', doc: ID }])
    // No secret: the API refuses (401), and that is "fetch a new token", never access.
    await assert.rejects(authorizeDoc({ api: backend.api, backendUrl: 'http://api.test', token: token('reader'), id: ID, secret: '' }), (e) => e.reason === REFUSED.unauthenticated)
})

test('an API from before the session still lets editors in, and only them', async () => {
    const backend = new FakeBackend()
    backend.legacy = true
    backend.levels.set('reader', 'view')
    assert.deepEqual(await authorizeDoc({ api: backend.api, backendUrl: 'http://api.test', token: token('editor'), id: ID, secret: 's' }), { level: 'edit', canEdit: true, locked: false })
    await assert.rejects(authorizeDoc({ api: backend.api, backendUrl: 'http://api.test', token: token('reader'), id: ID, secret: 's' }), (e) => e.reason === REFUSED.forbidden)
})

test('the messages a tab is told', () => {
    const access = { level: 'edit', canEdit: true, locked: false }
    assert.deepEqual(JSON.parse(sessionMessage({ access, lineage: 'L1', schemaVersion: 'S' })), { type: 'session', level: 'edit', canEdit: true, locked: false, lineage: 'L1', schemaVersion: 'S' })
    // A connection the server made read-only never says it may write.
    assert.equal(JSON.parse(sessionMessage({ access, readOnly: true })).canEdit, false)
    // What the save path tells every tab after a store (persisted.js): the
    // time in ms, as the doc session reads it, with the stored state vector
    // and lineage.
    assert.deepEqual(JSON.parse(persistedMessage({ at: 1700000000000, stateVector: new Uint8Array([0]), lineage: 'L2' })), { type: 'persisted', at: 1700000000000, sv: 'AA==', lineage: 'L2' })
})

// ---------------------------------------------------------------------------
// Live, over websockets
// ---------------------------------------------------------------------------

test('a reader joins live, read-only: sees typing within a second, cannot write, is told so', async () => {
    const { redis, backend } = world()
    backend.levels.set('viewer', 'view')
    backend.levels.set('commenter', 'comment')
    const collab = await startServer({ redis, backend })

    const editor = new TestClient({ url: collab.url, name: ID, token: token('editor') })
    assert.equal(await editor.connect(), 'synced')
    assert.equal(editor.scope, 'read-write')
    await until(() => sessionOf(editor), 2000, "the editor's session")
    assert.deepEqual({ ...sessionOf(editor), lineage: undefined }, { type: 'session', level: 'edit', canEdit: true, locked: false, lineage: undefined, schemaVersion: SCHEMA_HASH })

    for (const who of ['viewer', 'commenter']) {
        const reader = new TestClient({ url: collab.url, name: ID, token: token(who) })
        assert.equal(await reader.connect(), 'synced', who)
        assert.equal(reader.scope, 'readonly', who)
        await until(() => sessionOf(reader), 2000, `the ${who}'s session`)
        assert.equal(sessionOf(reader).canEdit, false)
        assert.equal(sessionOf(reader).level, who === 'viewer' ? 'view' : 'comment')
        // The same copy as the editor's.
        assert.equal(sessionOf(reader).lineage, sessionOf(editor).lineage)
        assert.match(text(reader.doc), /last Tuesday/)

        // The editor types; the reader sees it live.
        const t0 = Date.now()
        const p = new Y.XmlElement('paragraph')
        p.insert(0, [new Y.XmlText(`typed for the ${who}`)])
        editor.doc.getXmlFragment('default').push([p])
        await until(() => text(reader.doc).includes(`typed for the ${who}`), 1000, `the ${who} to see the typing`)
        assert.ok(Date.now() - t0 < 1000)

        // The reader's own writes never reach the doc.
        const q = new Y.XmlElement('paragraph')
        q.insert(0, [new Y.XmlText(`written by the ${who}`)])
        reader.doc.getXmlFragment('default').push([q])
        await sleep(100)
        assert.equal(count(text(collab.server.hocuspocus.documents.get(ID)), `written by the ${who}`), 0)
        assert.equal(count(text(editor.doc), `written by the ${who}`), 0)
        await reader.close()
        reader.destroy()
    }
    await editor.close()
    editor.destroy()
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('someone who may not read, or a deleted doc, is refused with its reason', async () => {
    const { redis, backend } = world()
    backend.levels.set('stranger', 'none')
    backend.levels.set('late', 'gone')
    const collab = await startServer({ redis, backend })
    for (const [who, reason] of [['stranger', REFUSED.forbidden], ['late', REFUSED.gone]]) {
        const tab = new TestClient({ url: collab.url, name: ID, token: token(who) })
        assert.equal(await tab.connect(), 'denied', who)
        assert.equal(tab.denied, reason)
        assert.equal(tab.doc.getXmlFragment('default').length, 0, 'nothing of the doc reached them')
        await tab.close()
        tab.destroy()
    }
    await collab.stop()
    assert.deepEqual(rejections, [])
})

test('against an API from before the session, an editor edits and a reader is refused as before', async () => {
    const { redis, backend } = world()
    backend.legacy = true
    backend.levels.set('viewer', 'view')
    const collab = await startServer({ redis, backend })
    const editor = new TestClient({ url: collab.url, name: ID, token: token('editor') })
    assert.equal(await editor.connect(), 'synced')
    assert.equal(editor.scope, 'read-write')
    const viewer = new TestClient({ url: collab.url, name: ID, token: token('viewer') })
    assert.equal(await viewer.connect(), 'denied')
    assert.equal(viewer.denied, REFUSED.forbidden)
    for (const c of [editor, viewer]) {
        await c.close()
        c.destroy()
    }
    await collab.stop()
    assert.deepEqual(rejections, [])
})

// Two clients, open, type, reconnect: the doc stays one copy, on the server
// and in both tabs (G-COLLAB's "one copy" test).
test('two clients typing and reconnecting keep one copy', async () => {
    const { redis, backend } = world()
    backend.levels.set('viewer', 'view')
    const collab = await startServer({ redis, backend })
    const editor = new TestClient({ url: collab.url, name: ID, token: token('editor') })
    const viewer = new TestClient({ url: collab.url, name: ID, token: token('viewer') })
    assert.equal(await editor.connect(), 'synced')
    assert.equal(await viewer.connect(), 'synced')
    for (let round = 0; round < 5; round++) {
        const p = new Y.XmlElement('paragraph')
        p.insert(0, [new Y.XmlText(`round ${round}`)])
        editor.doc.getXmlFragment('default').push([p])
        await until(() => text(viewer.doc).includes(`round ${round}`), 1000, `round ${round} at the viewer`)
        await editor.close()
        await viewer.close()
        assert.equal(await editor.connect(), 'synced')
        assert.equal(await viewer.connect(), 'synced')
    }
    // Saves have run; the server and both tabs hold each block once.
    await until(() => backend.saves.some((s) => s.html?.includes('round 4')), 3000, 'the save')
    for (const doc of [collab.server.hocuspocus.documents.get(ID), editor.doc, viewer.doc]) {
        const t = text(doc)
        assert.equal(count(t, 'last Tuesday'), 1)
        for (let round = 0; round < 5; round++) assert.equal(count(t, `round ${round}`), 1)
    }
    for (const c of [editor, viewer]) {
        await c.close()
        c.destroy()
    }
    await collab.stop()
    assert.deepEqual(rejections, [])
})

// ---------------------------------------------------------------------------
// One socket per tab: several docs over one websocket, each authorised alone
// ---------------------------------------------------------------------------

const varUint = (out, n) => {
    while (n > 0x7f) {
        out.push((n & 0x7f) | 0x80)
        n = Math.floor(n / 128)
    }
    out.push(n)
}
const varString = (out, s) => {
    const b = Buffer.from(s, 'utf8')
    varUint(out, b.length)
    out.push(...b)
}
function frame(name, type, write) {
    const out = []
    varString(out, name)
    varUint(out, type)
    write(out)
    return Buffer.from(out)
}
function readFrame(buf) {
    let i = 0
    const uint = () => {
        let n = 0
        let m = 1
        for (;;) {
            const x = buf[i++]
            n += (x & 0x7f) * m
            if (x < 0x80) return n
            m *= 128
        }
    }
    const str = () => {
        const n = uint()
        const s = Buffer.from(buf.subarray(i, i + n)).toString('utf8')
        i += n
        return s
    }
    const name = str()
    const type = uint()
    if (type === 2) {
        const sub = uint()
        return { name, type, sub, text: str() }
    }
    if (type === 5) return { name, type, text: str() }
    return { name, type }
}

test('one websocket carries several docs, each with its own decision and session', async () => {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc('doc-a', '<p>A</p>')
    backend.doc('doc-b', '<p>B</p>')
    backend.doc('doc-c', '<p>C</p>')
    backend.levels.set('sam', 'edit')
    const collab = await startServer({ redis, backend })
    // Sam edits doc-a, reads doc-b (a per-doc answer) and may not see doc-c.
    const levels = { 'doc-a': 'edit', 'doc-b': 'view', 'doc-c': 'none' }
    const post = backend.api.post
    backend.api.post = async (url, body, config) => {
        if (new URL(url).pathname === '/docColab/session') backend.levels.set('sam', levels[body.doc_uuid])
        return post(url, body, config)
    }

    const ws = new WebSocket(collab.url)
    const seen = []
    ws.on('message', (data) => seen.push(readFrame(Buffer.from(data))))
    await new Promise((resolve) => ws.on('open', resolve))
    for (const name of Object.keys(levels)) {
        ws.send(frame(name, 2, (o) => { varUint(o, 0); varString(o, token('sam')) }))
        ws.send(frame(name, 0, (o) => { varUint(o, 0); const sv = Y.encodeStateVector(new Y.Doc()); varUint(o, sv.length); o.push(...sv) }))
    }
    await until(() => seen.filter((m) => m.type === 2).length >= 3 && seen.filter((m) => m.type === 5).length >= 2, 3000, 'three answers')
    const auth = (name) => seen.find((m) => m.name === name && m.type === 2)
    const session = (name) => JSON.parse(seen.find((m) => m.name === name && m.type === 5).text)
    assert.equal(auth('doc-a').sub, 2)
    assert.equal(auth('doc-a').text, 'read-write')
    assert.equal(session('doc-a').canEdit, true)
    assert.equal(auth('doc-b').sub, 2)
    assert.equal(auth('doc-b').text, 'readonly')
    assert.equal(session('doc-b').canEdit, false)
    assert.equal(auth('doc-c').sub, 1, 'doc-c is refused')
    assert.equal(auth('doc-c').text, REFUSED.forbidden)
    // The refusal closed nothing else: doc-a still answers on the same socket.
    assert.equal(ws.readyState, WebSocket.OPEN)
    await new Promise((resolve) => {
        ws.once('close', resolve)
        ws.close()
    })
    await collab.stop()
    assert.deepEqual(rejections, [])
})
