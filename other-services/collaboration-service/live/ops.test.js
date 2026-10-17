import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { Extension } from '@tiptap/core'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { COLLAB_EXTENSIONS } from '../extensions.js'
import { covers } from '../persisted.js'
import { FakeBackend, FakeRedis, TestClient, onClose, startServer, until, sleep } from '../testkit.js'
import { createLiveServer } from './server.js'
import { AGENT_EDITS, runWrite } from './ops/write.js'

// The Live Doc Writer over a real collaboration server: a fake Redis and API,
// real websockets, and the internal port. Every server-side write is one
// transaction on the live copy, so a tab that has the doc open sees it arrive,
// nothing anyone types is overwritten, and no reconnect can double the doc
// (invariants I1–I3, I7, I8, I11).

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

const ID = '3c1d9b7e-2a4f-4e8b-9c6d-5f7a8b9c0d1e'
const BODY = '<h1>Launch</h1><p>Ship on Thursday.</p><p>Owners: Sam.</p>'
const SECRET = 'test-secret'

const json = (doc) => JSON.stringify(TiptapTransformer.fromYdoc(doc, 'default'))
const times = (doc, text) => json(doc).split(text).length - 1
const blocks = (doc) => doc.getXmlFragment('default').length

// Block ids, as A1 will add them: an attribute on every top-level block.
const BlockIds = Extension.create({
    name: 'blockIdsForTest',
    addGlobalAttributes() {
        return [{
            types: ['paragraph', 'heading'],
            attributes: { blockId: { default: null, parseHTML: (el) => el.getAttribute('data-block-id'), renderHTML: (a) => (a.blockId ? { 'data-block-id': a.blockId } : {}) } },
        }]
    },
})

async function world({ body = BODY, extensions } = {}) {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(ID, body)
    const collab = await startServer({ redis, backend, ...(extensions ? { extensions } : {}) })
    const live = createLiveServer({ getSecret: () => SECRET, hocuspocus: collab.server.hocuspocus, deps: collab.live })
    await new Promise((resolve) => live.listen(0, '127.0.0.1', resolve))
    // Closed when the test ends, unless close() below gets there first: an op
    // still waiting on the doc is cut off rather than held open.
    const stopLive = onClose(() => new Promise((resolve) => {
        live.close(resolve)
        live.closeAllConnections()
    }))
    const base = `http://127.0.0.1:${live.address().port}`
    const op = async (name, payload, doc = ID) => {
        const res = await fetch(`${base}/live/docs/${doc}/${name}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'X-Internal-Secret': SECRET },
            body: JSON.stringify(payload),
        })
        const out = await res.json()
        return { status: res.status, ...out }
    }
    const tabs = []
    const tab = async (token = '{"sub":"user-1"}') => {
        const t = new TestClient({ url: collab.url, name: ID, token })
        assert.equal(await t.connect(), 'synced')
        tabs.push(t)
        return t
    }
    const close = async () => {
        for (const t of tabs) {
            await t.close()
            t.destroy()
        }
        await stopLive()
        await collab.stop()
    }
    return { redis, backend, collab, op, tab, close, base }
}

test('each write arrives in an open tab as one transaction, and is stored', async () => {
    const w = await world()
    const tab = await w.tab()
    const doc = w.collab.server.hocuspocus.documents.get(ID)
    for (const [name, payload, expect] of [
        ['append', { html: '<p>Appended.</p>' }, 'Appended.'],
        ['replaceAll', { html: '<h1>Launch</h1><p>Ship on Friday.</p><p>Owners: Sam.</p>' }, 'Ship on Friday.'],
    ]) {
        let updates = 0
        const count = () => updates++
        doc.on('update', count)
        const out = await w.op(name, { ...payload, by: { user_uuid: 'user-9' } })
        doc.off('update', count)
        assert.equal(out.status, 200, `${name}: ${out.msg}`)
        assert.equal(updates, 1, `${name} is one transaction`)
        assert.equal(out.data.persisted, true, `${name} is stored`)
        assert.ok(out.data.persisted_at)
        await until(() => times(tab.doc, expect) === 1, 2000, `${name} in the tab`)
        assert.match(w.backend.docs.get(ID).doc_body, new RegExp(expect.replace('.', '\\.')))
    }
    // replaceAll kept the blocks that read the same: the heading and the
    // owners line are the same Yjs items as before.
    assert.equal(times(tab.doc, 'Ship on Thursday.'), 0)
    assert.equal(times(tab.doc, 'Launch'), 1)
    await w.close()
    assert.deepEqual(rejections, [])
})

test('a restore made while tabs are connected shows in each within 2 s, once, and survives the next save', async () => {
    const w = await world()
    const tab = await w.tab()
    const other = await w.tab('{"sub":"user-3"}')
    const restored = '<h1>Launch</h1><p>Restored: we launch in March.</p>'
    const out = await w.op('replaceAll', { html: restored, by: { user_uuid: 'user-2' }, snapshot_first: true })
    assert.equal(out.status, 200, out.msg)
    // The version the API keeps first is exactly what the restore replaced.
    assert.match(out.data.before.html, /Ship on Thursday\./)
    assert.match(out.data.before.html, /Owners: Sam\./)
    // Both tabs (two people's browsers) have it within 2 s, and no block twice.
    for (const t of [tab, other]) {
        await until(() => times(t.doc, 'Restored: we launch in March.') === 1, 2000, 'the restore in each tab')
        assert.equal(times(t.doc, 'Ship on Thursday.'), 0)
        assert.equal(times(t.doc, 'Launch'), 1)
        assert.equal(blocks(t.doc), 2)
    }

    // The tab keeps typing; its save carries the restore, never the old body.
    const p = new Y.XmlElement('paragraph')
    p.insert(0, [new Y.XmlText('Typed after the restore.')])
    tab.doc.getXmlFragment('default').insert(tab.doc.getXmlFragment('default').length, [p])
    await until(() => w.backend.docs.get(ID).doc_body.includes('Typed after the restore.'), 3000, 'the next save')
    const stored = w.backend.docs.get(ID).doc_body
    assert.match(stored, /Restored: we launch in March\./)
    assert.doesNotMatch(stored, /Ship on Thursday/)
    await w.close()
    assert.deepEqual(rejections, [])
})

test('a tab that slept through a server-side change reconnects to one copy', async () => {
    const w = await world()
    const tab = await w.tab()
    // Two tabs on the same doc: one stays, one sleeps (a laptop lid closed).
    const awake = await w.tab('{"sub":"user-2"}')
    await tab.close()
    // The doc changes underneath the sleeping tab, through the live copy.
    assert.equal((await w.op('replaceAll', { html: '<h1>Launch</h1><p>Moved to March.</p>', by: { user_uuid: 'user-2' } })).status, 200)
    // It wrote offline meanwhile, at the end.
    const p = new Y.XmlElement('paragraph')
    p.insert(0, [new Y.XmlText('Written offline.')])
    tab.doc.getXmlFragment('default').insert(tab.doc.getXmlFragment('default').length, [p])
    // Ten reconnects later the doc holds each block once, everywhere.
    for (let round = 0; round < 10; round++) {
        assert.equal(await tab.connect(), 'synced', `round ${round}`)
        await sleep(20)
        await tab.close()
    }
    assert.equal(await tab.connect(), 'synced')
    const live = w.collab.server.hocuspocus.documents.get(ID)
    await until(() => times(awake.doc, 'Written offline.') === 1, 2000, 'the offline edit in the other tab')
    for (const doc of [live, tab.doc, awake.doc]) {
        assert.equal(times(doc, 'Moved to March.'), 1)
        assert.equal(times(doc, 'Launch'), 1)
        assert.equal(times(doc, 'Written offline.'), 1)
        assert.equal(times(doc, 'Ship on Thursday.'), 0)
        assert.equal(blocks(doc), 3)
    }
    assert.equal(tab.stateless.filter((m) => JSON.parse(m).type === 'epoch-changed').length, 0, 'the same lineage: nothing to drop')
    await w.close()
    assert.deepEqual(rejections, [])
})

test('two tabs typing while the server writes keep one copy of every block', async () => {
    const w = await world()
    const a = await w.tab('{"sub":"user-a"}')
    const b = await w.tab('{"sub":"user-b"}')
    const write = (t, text) => {
        const p = new Y.XmlElement('paragraph')
        p.insert(0, [new Y.XmlText(text)])
        t.doc.getXmlFragment('default').insert(t.doc.getXmlFragment('default').length, [p])
    }
    write(a, 'From A.')
    const ops = [
        w.op('append', { html: '<p>From the server.</p>', by: { user_uuid: 'user-c' } }),
        w.op('seed', { html: '<p>Never.</p>' }),
    ]
    write(b, 'From B.')
    const [appended, seeded] = await Promise.all(ops)
    assert.equal(appended.status, 200)
    assert.equal(seeded.status, 409, 'a doc with content is never seeded')
    const live = w.collab.server.hocuspocus.documents.get(ID)
    await until(() => ['From A.', 'From B.', 'From the server.'].every((t) => times(a.doc, t) === 1 && times(b.doc, t) === 1), 3000, 'every edit everywhere')
    for (const doc of [live, a.doc, b.doc]) {
        for (const t of ['Launch', 'Ship on Thursday.', 'From A.', 'From B.', 'From the server.']) assert.equal(times(doc, t), 1, t)
        assert.equal(times(doc, 'Never.'), 0)
    }
    // A reconnect of each changes nothing.
    for (const t of [a, b]) {
        await t.close()
        assert.equal(await t.connect(), 'synced')
    }
    await sleep(50)
    assert.equal(blocks(live), 6)
    await w.close()
    assert.deepEqual(rejections, [])
})

test('seed writes a new doc once, and refuses one that has content', async () => {
    const w = await world({ body: '' })
    const first = await w.op('seed', { html: '<h1>From the template</h1><p>Fill this in.</p>', by: { user_uuid: 'user-1' } })
    assert.equal(first.status, 200, first.msg)
    assert.equal(first.data.blocks, 2)
    assert.match(w.backend.docs.get(ID).doc_body, /From the template/)
    const second = await w.op('seed', { html: '<h1>From the template</h1><p>Fill this in.</p>' })
    assert.equal(second.status, 409)
    const tab = await w.tab()
    assert.equal(times(tab.doc, 'From the template'), 1)
    await w.close()
})

test('writes are credited to whoever the API names, and an agent with whoever asked it', async () => {
    const w = await world()
    const tab = await w.tab()
    const out = await w.op('append', { html: '<p>Rollback: revert the deploy.</p>', by: { user_uuid: 'sam', agent_uuid: 'release-captain' }, agent_edit: true, run_id: 'run-7' })
    assert.equal(out.status, 200, out.msg)
    assert.ok(out.data.edit_id)
    const saved = w.backend.saves.find((s) => s.html?.includes('Rollback'))
    assert.deepEqual(saved.agents, [{ agent_uuid: 'release-captain', asker_uuid: 'sam' }])
    assert.deepEqual(saved.contributors, [], 'the agent wrote it; the person asked')
    // The range the agent wrote, beside the body, for every tab to mark.
    await until(() => tab.doc.getMap(AGENT_EDITS).size === 1, 2000, 'the agent edit record in the tab')
    const edit = tab.doc.getMap(AGENT_EDITS).get(out.data.edit_id)
    assert.equal(edit.agent, 'release-captain')
    assert.equal(edit.asker, 'sam')
    assert.equal(edit.run, 'run-7')
    const from = Y.createAbsolutePositionFromRelativePosition(Y.decodeRelativePosition(Buffer.from(edit.from, 'base64')), tab.doc)
    const to = Y.createAbsolutePositionFromRelativePosition(Y.decodeRelativePosition(Buffer.from(edit.to, 'base64')), tab.doc)
    assert.deepEqual([from.index, to.index], [3, 4], 'the appended block, and only it')
    // Someone typing is credited as themselves.
    const p = new Y.XmlElement('paragraph')
    p.insert(0, [new Y.XmlText('Typed by user-1.')])
    tab.doc.getXmlFragment('default').insert(0, [p])
    await until(() => w.backend.saves.some((s) => s.html?.includes('Typed by user-1.')), 3000, 'the save')
    assert.deepEqual(w.backend.saves.find((s) => s.html?.includes('Typed by user-1.')).contributors, ['user-1'])
    await w.close()
})

test('blocks are found by their id: insertAfter and replaceBlocks', async () => {
    const extensions = [...COLLAB_EXTENSIONS, BlockIds]
    const w = await world({ body: '<h1 data-block-id="b-title">Launch</h1><p data-block-id="b-ship">Ship on Thursday.</p><p data-block-id="b-own">Owners: Sam.</p>', extensions })
    const tab = await w.tab()
    const inserted = await w.op('insertAfter', { block_id: 'b-title', html: '<p>Status: green.</p>' })
    assert.equal(inserted.status, 200, inserted.msg)
    assert.equal(inserted.data.block_ids.length, 1, 'a new block gets an id')
    const replaced = await w.op('replaceBlocks', { block_ids: ['b-ship'], html: '<p>Ship on Friday.</p><p>QA signs off first.</p>' })
    assert.equal(replaced.status, 200, replaced.msg)
    assert.equal(replaced.data.removed, 1)
    assert.equal((await w.op('insertAfter', { block_id: 'nope', html: '<p>x</p>' })).status, 404)
    assert.equal((await w.op('replaceBlocks', { block_ids: ['b-own', 'nope'], html: '<p>x</p>' })).status, 404)
    await until(() => times(tab.doc, 'QA signs off first.') === 1, 2000, 'the replacement in the tab')
    const texts = tab.doc.getXmlFragment('default').toArray().map((b) => b.toArray().map((t) => t.toString()).join(''))
    assert.deepEqual(texts, ['Launch', 'Status: green.', 'Ship on Friday.', 'QA signs off first.', 'Owners: Sam.'])
    await w.close()
})

test('a write whose save fails says so, and the save is retried', async () => {
    const w = await world()
    w.backend.saveStatus = 503
    const out = await w.op('append', { html: '<p>While the API was down.</p>' })
    assert.equal(out.status, 200)
    assert.equal(out.data.persisted, false)
    assert.equal(out.data.persisted_at, null)
    w.backend.saveStatus = null
    await until(() => w.backend.docs.get(ID).doc_body.includes('While the API was down.'), 5000, 'the retried save')
    await w.close()
    assert.deepEqual(rejections, [])
})

test('saved means stored: tabs hear when their edits are in the database', async () => {
    const w = await world()
    const tab = await w.tab()
    // On connecting, the tab hears what is stored already (here: nothing yet).
    const onConnect = tab.stateless.filter((m) => JSON.parse(m).type === 'persisted').length
    w.backend.saveStatus = 503
    const p = new Y.XmlElement('paragraph')
    p.insert(0, [new Y.XmlText('Mine.')])
    tab.doc.getXmlFragment('default').insert(0, [p])
    await until(() => w.collab.logs.lines.some((l) => l.includes('SAVE FAILED')), 3000, 'a failed save')
    assert.equal(tab.stateless.filter((m) => JSON.parse(m).type === 'persisted').length, onConnect, 'nothing stored, nothing said')
    w.backend.saveStatus = null
    await until(() => tab.stateless.filter((m) => JSON.parse(m).type === 'persisted').length > onConnect, 5000, 'the persisted message')
    const msg = JSON.parse(tab.stateless.filter((m) => JSON.parse(m).type === 'persisted').at(-1))
    // The time in ms, as the doc session reads it (web lib/collabSession.ts).
    assert.ok(Number.isInteger(msg.at) && msg.at > 0)
    assert.equal(covers(msg.sv, Y.encodeStateVector(tab.doc)), true, 'it covers everything this tab wrote')
    assert.match(w.backend.docs.get(ID).doc_body, /Mine\./)
    await w.close()
    assert.deepEqual(rejections, [])
})

test('reauth closes the sockets of the people named, and they come back authorised afresh', async () => {
    const w = await world()
    const sam = await w.tab('{"sub":"sam"}')
    const priya = await w.tab('{"sub":"priya"}')
    const out = await w.op('reauth', { user_uuids: ['sam'] })
    assert.equal(out.data.closed, 1)
    await until(() => sam.ws.readyState === 3, 2000, "sam's socket to close")
    sam.ws = null
    assert.equal(priya.ws.readyState, 1, "priya's stays")
    // Reconnecting is authorising again (the provider does this by itself).
    assert.equal(await sam.connect(), 'synced')
    assert.equal((await w.op('reauth', {})).data.closed, 2, 'everyone, when nobody is named')
    await until(() => sam.ws.readyState === 3 && priya.ws.readyState === 3, 2000, 'both sockets to close')
    sam.ws = null
    priya.ws = null
    await w.close()
})

test('read answers the live doc, typing included; render writes a state as HTML', async () => {
    const w = await world()
    const tab = await w.tab()
    const p = new Y.XmlElement('paragraph')
    p.insert(0, [new Y.XmlText('Not saved yet.')])
    tab.doc.getXmlFragment('default').insert(0, [p])
    await until(() => times(w.collab.server.hocuspocus.documents.get(ID), 'Not saved yet.') === 1, 2000, 'the edit on the server')
    const out = await w.op('read', {})
    assert.equal(out.status, 200)
    assert.match(out.data.html, /Not saved yet\./)
    assert.equal(out.data.open, true)
    assert.ok(out.data.hash)
    const state = Buffer.from(Y.encodeStateAsUpdate(tab.doc)).toString('base64')
    const rendered = await w.op('render', { state })
    assert.match(rendered.data.html, /Not saved yet\./)
    assert.equal(rendered.data.open, true)
    assert.equal((await w.op('render', { state: 'not a state' })).status, 422)
    await w.close()
})

test('the public port takes no server-side writes; the internal one needs the secret', async () => {
    const w = await world()
    const pub = w.collab.url.replace('ws://', 'http://')
    for (const path of [`/internal/docs/${ID}/append`, `/live/docs/${ID}/append`, `/internal/docs/${ID}/render`]) {
        const res = await fetch(pub + path, { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Internal-Secret': SECRET }, body: '{"html":"<p>x</p>"}' })
        assert.equal(res.status, 404, path)
    }
    const noSecret = await fetch(`${w.base}/live/docs/${ID}/append`, { method: 'POST', body: '{"html":"<p>x</p>"}' })
    assert.equal(noSecret.status, 401)
    assert.doesNotMatch(w.backend.docs.get(ID).doc_body, /<p>x<\/p>/)
    await w.close()
})

test('a doc that cannot be read is never written into', async () => {
    const w = await world()
    w.backend.getStatus = 503
    const out = await w.op('append', { html: '<p>Into nothing.</p>' })
    assert.equal(out.status, 503)
    w.backend.getStatus = null
    const missing = await w.op('append', { html: '<p>x</p>' }, '0d4f1a2b-0000-4000-8000-000000000000')
    assert.equal(missing.status, 404)
    await w.close()
    assert.deepEqual(rejections, [])
})

// An op built on runWrite (applyProposal is one, A9) gets the fields its apply
// returns back in the answer, beside the write's own.
test("an op's own fields come back with the write's answer", async () => {
    const w = await world()
    const routes = new Map([['mark', (args) => runWrite(args, {
        build: () => null,
        locate: () => null,
        apply: (doc) => {
            const body = doc.getXmlFragment('default')
            const p = new Y.XmlElement('paragraph')
            p.insert(0, [new Y.XmlText('Marked.')])
            body.insert(body.length, [p])
            return { from: body.length - 1, to: body.length, blocks: 1, applied: ['h1'] }
        },
    })]])
    const live = createLiveServer({ getSecret: () => SECRET, routes, hocuspocus: w.collab.server.hocuspocus, deps: w.collab.live })
    await new Promise((resolve) => live.listen(0, '127.0.0.1', resolve))
    const stop = onClose(() => new Promise((resolve) => {
        live.close(resolve)
        live.closeAllConnections()
    }))
    const res = await fetch(`http://127.0.0.1:${live.address().port}/live/docs/${ID}/mark`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Internal-Secret': SECRET },
        body: JSON.stringify({ by: { user_uuid: 'user-2' } }),
    })
    const out = await res.json()
    assert.equal(res.status, 200, out.msg)
    assert.deepEqual(out.data.applied, ['h1'])
    assert.equal(out.data.blocks, 1)
    assert.equal(out.data.from, undefined)
    await stop()
    await w.close()
    assert.deepEqual(rejections, [])
})

// A doc that went to the trash while it was open is let go once its save is
// refused as gone (saveFailures.js): a server-side write into it is refused
// too, never made where nothing could store it.
test('a doc let go as gone takes no server-side write', async () => {
    const w = await world()
    const tab = await w.tab()
    w.backend.saveStatus = 410
    const p = new Y.XmlElement('paragraph')
    p.insert(0, [new Y.XmlText('Typed after the delete.')])
    tab.doc.getXmlFragment('default').insert(0, [p])
    await until(() => w.collab.failures.isGone(ID), 3000, 'the doc let go as gone')
    const out = await w.op('append', { html: '<p>A server-side write.</p>', by: { user_uuid: 'user-2' } })
    assert.equal(out.status, 410, out.msg)
    assert.equal(times(w.collab.server.hocuspocus.documents.get(ID), 'A server-side write.'), 0)
    await w.close()
    assert.deepEqual(rejections, [])
})
