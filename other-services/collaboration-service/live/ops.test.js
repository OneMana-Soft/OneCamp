import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { Extension } from '@tiptap/core'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { COLLAB_EXTENSIONS } from '../extensions.js'
import { bodyHash } from '../docState.js'
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
const BOARD = '7e2a9c41-5b3d-4f6e-8a1b-2c3d4e5f6a7b'
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

// world is a collaboration server and its live port. server is more options
// for the server (its save debounce), and liveDeps(deps) the deps the live
// port's ops get, from the server's own.
async function world({ body = BODY, extensions, server = {}, liveDeps = (deps) => deps } = {}) {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(ID, body)
    const collab = await startServer({ redis, backend, ...(extensions ? { extensions } : {}), ...server })
    const live = createLiveServer({ getSecret: () => SECRET, hocuspocus: collab.server.hocuspocus, deps: liveDeps(collab.live) })
    await new Promise((resolve) => live.listen(0, '127.0.0.1', resolve))
    // Closed when the test ends, unless close() below gets there first: an op
    // still waiting on the doc is cut off rather than held open.
    const stopLive = onClose(() => new Promise((resolve) => {
        live.close(resolve)
        live.closeAllConnections()
    }))
    const base = `http://127.0.0.1:${live.address().port}`
    const post = async (path, payload) => {
        const res = await fetch(`${base}${path}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'X-Internal-Secret': SECRET },
            body: JSON.stringify(payload),
        })
        const out = await res.json()
        return { status: res.status, ...out }
    }
    const op = (name, payload, doc = ID) => post(`/live/docs/${doc}/${name}`, payload)
    // An op on the world's board, and one about people rather than a doc.
    const board = (name, payload) => post(`/live/boards/${BOARD}/${name}`, payload)
    const people = (name, payload) => post(`/live/people/${name}`, payload)
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
    // A tab on the world's board (BOARD), opened and synced.
    const boardTab = async (token) => {
        if (!backend.boards.has(BOARD)) backend.boards.set(BOARD, { board_state: '' })
        const t = new TestClient({ url: collab.url, name: `board:${BOARD}`, token })
        assert.equal(await t.connect(), 'synced')
        return t
    }
    return { redis, backend, collab, op, board, people, tab, boardTab, close, base }
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

// A quiet write (a copy's body, an import's) is saved saying so: the API keeps
// whom its body mentions and tells nobody. Whoever the API named still counts
// among who edited. Typing after it is a person's own, and its save is loud.
test('a quiet seed is saved as quiet, and the typing after it is not', async () => {
    const w = await world({ body: '' })
    const seeded = await w.op('seed', { html: '<p>Copied: Maya owns the launch.</p>', by: { user_uuid: 'user-1' }, quiet: true })
    assert.equal(seeded.status, 200, seeded.msg)
    const copied = w.backend.saves.find((s) => s.html?.includes('Copied'))
    assert.ok(copied, 'the seed is stored')
    assert.equal(copied.quiet, true, "the seed's save says it is quiet")
    assert.deepEqual(copied.contributors, ['user-1'], 'a quiet person still counts among who edited')

    const tab = await w.tab('{"sub":"user-2"}')
    const p = new Y.XmlElement('paragraph')
    p.insert(0, [new Y.XmlText('Typed after the copy.')])
    tab.doc.getXmlFragment('default').insert(tab.doc.getXmlFragment('default').length, [p])
    await until(() => w.backend.saves.some((s) => s.html?.includes('Typed after the copy.')), 3000, 'the save of the typing')
    const typed = w.backend.saves.find((s) => s.html?.includes('Typed after the copy.'))
    assert.equal(typed.quiet, false, 'typing is never quiet')
    assert.deepEqual(typed.contributors, ['user-2'])
    await w.close()
    assert.deepEqual(rejections, [])
})

// A quiet write's save stands alone: the op stores the doc at once (the direct
// connection's transact), not on the save debounce, and answers once it is
// stored. So a person who opens the fresh copy and types an @mention within
// the debounce (5 s here, as in production) is saved apart and loudly: the API
// tells whom they mention (Ana), not whom the seed does (Maya).
test('a quiet seed is stored at once, so typing within the debounce is saved apart and loud', async () => {
    const w = await world({ body: '', server: { debounce: 5000, maxDebounce: 30000 } })
    const seeded = await w.op('seed', { html: '<p>Copied: <span data-type="mention" data-id="maya@0x3" data-label="Maya">@Maya</span> owns the launch.</p>', by: { user_uuid: 'user-1' }, quiet: true })
    assert.equal(seeded.status, 200, seeded.msg)
    assert.equal(seeded.data.persisted, true, 'stored before the op answers')

    const tab = await w.tab('{"sub":"user-2"}')
    const p = new Y.XmlElement('paragraph')
    const ana = new Y.XmlElement('mention')
    ana.setAttribute('id', 'ana@0x5')
    ana.setAttribute('label', 'Ana')
    p.insert(0, [new Y.XmlText('Ask '), ana])
    tab.doc.getXmlFragment('default').insert(tab.doc.getXmlFragment('default').length, [p])
    await until(() => w.backend.saves.some((s) => s.html?.includes('ana@0x5')), 9000, 'the save of the typing')

    const seed = w.backend.saves.filter((s) => s.html?.includes('maya@0x3') && !s.html.includes('ana@0x5'))
    assert.equal(seed.length, 1, "the seed's own save")
    assert.equal(seed[0].quiet, true)
    for (const s of w.backend.saves.filter((x) => x.html?.includes('ana@0x5'))) {
        assert.equal(s.quiet, false, 'a save holding the typing is loud')
        assert.deepEqual(s.contributors, ['user-2'])
    }
    await w.close()
    assert.deepEqual(rejections, [])
})

// A copy's body that mentions Maya, as Duplicate seeds it, quietly.
const COPIED = '<p>Copied: <span data-type="mention" data-id="maya@0x3" data-label="Maya">@Maya</span> owns the launch.</p>'

// asking is a paragraph that mentions someone, as a person types it.
function asking(label, id) {
    const p = new Y.XmlElement('paragraph')
    const chip = new Y.XmlElement('mention')
    chip.setAttribute('id', id)
    chip.setAttribute('label', label)
    p.insert(0, [new Y.XmlText('Ask '), chip])
    return p
}

const serverDoc = (w) => w.collab.server.hocuspocus.documents.get(ID)

// gateRedis makes Redis slow to answer a save's first question, the buffer's
// count, which comes before the save reads the doc: it answers once ready()
// holds (or after 2 s). In production the save asks Redis twice before it
// reads the doc (the Redis extension's lock, then the count), so whatever
// reaches the doc while Redis answers is in the doc the save reads.
function gateRedis(w, ready) {
    const llen = w.redis.llen.bind(w.redis)
    w.redis.llen = async (key) => {
        await until(ready, 2000, 'the gate').catch(() => {})
        return llen(key)
    }
}

// A write to a doc nobody has open answers that it is stored once its save is
// in. Letting go of such a doc unloads it, and the answer used to be read after
// that, when the record of what is stored was gone with it: every such write
// answered persisted: false and logged a retry that never was.
test('a write to a doc nobody has open answers that it is stored, and logs no retry', async () => {
    const w = await world()
    const out = await w.op('append', { html: '<p>Appended with nobody there.</p>', by: { user_uuid: 'user-9' } })
    assert.equal(out.status, 200, out.msg)
    assert.match(w.backend.docs.get(ID).doc_body, /Appended with nobody there\./, 'stored before the op answered')
    assert.equal(w.collab.server.hocuspocus.documents.has(ID), false, 'let go of, as nobody has it open')
    assert.equal(out.data.persisted, true)
    assert.ok(out.data.persisted_at)
    assert.deepEqual(w.collab.logs.lines.filter((l) => l.includes('[Live]')), [], 'no retry logged')
    await w.close()
    assert.deepEqual(rejections, [])
})

// A quiet write nothing joins is one save.
test('a quiet seed nothing joins is saved once', async () => {
    const w = await world({ body: '' })
    const seeded = await w.op('seed', { html: COPIED, by: { user_uuid: 'user-1' }, quiet: true })
    assert.equal(seeded.status, 200, seeded.msg)
    assert.equal(seeded.data.persisted, true)
    await sleep(100) // the kit's save debounce is 20 ms: any later save is in by now
    assert.equal(w.backend.payloads.length, 1)
    assert.equal(w.backend.payloads[0].quiet, true)
    assert.deepEqual(w.backend.payloads[0].contributors, ['user-1'])
    await w.close()
    assert.deepEqual(rejections, [])
})

// What reaches a doc while its quiet save waits on Redis is not that save's.
// A tab open on the new doc types an @mention the moment the seed reaches it,
// and Redis answers only once that typing is on the server. The seed's save
// holds the doc as the seed left it, quiet and credited to the seed alone. The
// typing goes in the next save, loud and credited to the typist alone, and that
// save follows the seed's (its lineage) at the API.
test('typing that lands while a quiet save waits on Redis is saved after it, loudly', async () => {
    const w = await world({ body: '', server: { debounce: 5000, maxDebounce: 30000 } })
    const tab = await w.tab('{"sub":"user-2"}')
    let typed = false
    gateRedis(w, () => typed && covers(Y.encodeStateVector(serverDoc(w)), Y.encodeStateVector(tab.doc)))
    tab.doc.on('update', (_u, origin) => {
        if (typed || origin !== tab || !json(tab.doc).includes('maya@0x3')) return
        typed = true
        tab.doc.getXmlFragment('default').push([asking('Ana', 'ana@0x5')])
    })
    const seeded = await w.op('seed', { html: COPIED, by: { user_uuid: 'user-1' }, quiet: true })
    assert.equal(seeded.status, 200, seeded.msg)
    assert.ok(typed, 'the tab typed while the seed was being saved')
    assert.equal(seeded.data.persisted, true)
    await until(() => w.backend.payloads.some((p) => p.htmlContent?.includes('ana@0x5')), 9000, 'the save of the typing')

    const [seed, typing, ...more] = w.backend.payloads
    assert.equal(seed.quiet, true, "the seed's save is quiet")
    assert.deepEqual(seed.contributors, ['user-1'], 'and credited to the seed alone')
    assert.match(seed.htmlContent, /maya@0x3/)
    assert.doesNotMatch(seed.htmlContent, /ana@0x5/, 'and holds only the seed')
    assert.ok(typing, 'the typing is saved after it')
    assert.equal(typing.quiet, undefined, 'loudly')
    assert.deepEqual(typing.contributors, ['user-2'], 'credited to the typist alone')
    assert.match(typing.htmlContent, /maya@0x3[\s\S]*ana@0x5/)
    assert.equal(typing.lineage, seed.lineage)
    assert.deepEqual(more, [], 'two saves')
    assert.match(w.backend.docs.get(ID).doc_body, /ana@0x5/)
    await w.close()
    assert.deepEqual(rejections, [])
})

// The save after a quiet one is checked against it at the API. The typing that
// lands while the quiet save waits is undone before that save reads the doc, so
// the body is the seed's again and only the state moved: the next save stores
// the state against the body the quiet save stored (its hash, its lineage), and
// the API takes it.
test("the save after a quiet one is checked against the quiet save's body", async () => {
    const w = await world({ body: '', server: { debounce: 5000, maxDebounce: 30000 } })
    const tab = await w.tab('{"sub":"user-2"}')
    let typed = false
    gateRedis(w, () => typed && covers(Y.encodeStateVector(serverDoc(w)), Y.encodeStateVector(tab.doc)))
    tab.doc.on('update', (_u, origin) => {
        if (typed || origin !== tab || !json(tab.doc).includes('maya@0x3')) return
        typed = true
        const fragment = tab.doc.getXmlFragment('default')
        fragment.push([asking('Ana', 'ana@0x5')])
        fragment.delete(fragment.length - 1, 1)
    })
    const seeded = await w.op('seed', { html: COPIED, by: { user_uuid: 'user-1' }, quiet: true })
    assert.equal(seeded.status, 200, seeded.msg)
    assert.ok(typed, 'the tab typed while the seed was being saved')
    await until(() => w.backend.payloads.length >= 2, 3000, 'the save after the quiet one')

    const [seed, after, ...more] = w.backend.payloads
    assert.equal(seed.quiet, true)
    assert.deepEqual(seed.contributors, ['user-1'])
    assert.equal(after.keepBody, true, "the body is the seed's: only the state moved")
    assert.equal(after.expectBodyHash, bodyHash(seed.htmlContent), "checked against the quiet save's body")
    assert.equal(after.lineage, seed.lineage)
    assert.equal(after.quiet, undefined)
    assert.deepEqual(after.contributors, ['user-2'])
    assert.deepEqual(more, [])
    assert.deepEqual(w.backend.saves.map((s) => [!!s.keepBody, s.quiet]), [[false, true], [true, false]], 'the API took both, in order')
    await w.close()
    assert.deepEqual(rejections, [])
})

// A seed goes only into a doc nobody has written in, saved or not, so the doc
// as a quiet seed leaves it never holds anyone's typing. Typing still on the
// save debounce refuses the seed, and is saved as the typist's, loudly.
test("a quiet seed is refused by a doc holding someone's unsaved typing, which stays theirs", async () => {
    const w = await world({ body: '', server: { debounce: 5000, maxDebounce: 30000 } })
    const tab = await w.tab('{"sub":"user-2"}')
    tab.doc.getXmlFragment('default').push([asking('Ana', 'ana@0x5')])
    await until(() => json(serverDoc(w)).includes('ana@0x5'), 2000, 'the typing on the server')
    assert.equal(w.backend.payloads.length, 0, 'not saved yet: it is on the debounce')
    const seeded = await w.op('seed', { html: COPIED, by: { user_uuid: 'user-1' }, quiet: true })
    assert.equal(seeded.status, 409, seeded.msg)
    await until(() => w.backend.payloads.some((p) => p.htmlContent?.includes('ana@0x5')), 9000, 'the save of the typing')
    for (const p of w.backend.payloads) {
        assert.equal(p.quiet, undefined, 'loud')
        assert.deepEqual(p.contributors, ['user-2'])
        assert.doesNotMatch(p.htmlContent, /maya@0x3/)
    }
    await w.close()
    assert.deepEqual(rejections, [])
})

// A quiet save that fails is retried as it was: the doc as the quiet write left
// it, quiet and credited to it alone. What was typed while it failed is saved
// after it, loudly.
test('a quiet save that fails is retried quiet and alone, and the typing meanwhile after it, loudly', async () => {
    const w = await world({ body: '', server: { debounce: 5000, maxDebounce: 30000 } })
    w.backend.saveStatus = 503
    const seeded = await w.op('seed', { html: COPIED, by: { user_uuid: 'user-1' }, quiet: true })
    assert.equal(seeded.status, 200, seeded.msg)
    assert.equal(seeded.data.persisted, false, 'not stored yet')
    const tab = await w.tab('{"sub":"user-2"}')
    tab.doc.getXmlFragment('default').push([asking('Ana', 'ana@0x5')])
    await until(() => json(serverDoc(w)).includes('ana@0x5'), 2000, 'the typing on the server')
    w.backend.saveStatus = null
    await until(() => w.backend.payloads.some((p) => p.htmlContent?.includes('ana@0x5')), 9000, 'the save of the typing')

    const [seed, typing, ...more] = w.backend.payloads
    assert.equal(seed.quiet, true)
    assert.deepEqual(seed.contributors, ['user-1'])
    assert.doesNotMatch(seed.htmlContent, /ana@0x5/)
    assert.ok(typing, 'the typing is saved after it')
    assert.equal(typing.quiet, undefined)
    assert.deepEqual(typing.contributors, ['user-2'])
    assert.equal(typing.lineage, seed.lineage)
    assert.deepEqual(more, [])
    await w.close()
    assert.deepEqual(rejections, [])
})

// The one place the conservative choice remains (contributors.js): a quiet
// write whose save never went through before the service restarted. What the
// quiet write left was held in memory and is gone; its quiet mark is in Redis
// with the doc's kept state, and the save that takes it is quiet whatever it
// holds. So the copy's mentions still tell nobody, and nor does anything typed
// into it before the restart.
test('after a restart, a quiet write never saved is saved quiet, with what was typed before the restart', async () => {
    const w = await world({ body: '', server: { debounce: 5000, maxDebounce: 30000 } })
    w.backend.saveStatus = 503
    const seeded = await w.op('seed', { html: COPIED, by: { user_uuid: 'user-1' }, quiet: true })
    assert.equal(seeded.status, 200, seeded.msg)
    const tab = await w.tab('{"sub":"user-2"}')
    tab.doc.getXmlFragment('default').push([asking('Ana', 'ana@0x5')])
    await until(() => json(serverDoc(w)).includes('ana@0x5'), 2000, 'the typing on the server')
    await w.close()
    assert.equal(w.backend.payloads.length, 0, 'nothing stored before the restart')

    const collab = await startServer({ redis: w.redis, backend: w.backend, debounce: 5000, maxDebounce: 30000 })
    w.backend.saveStatus = null
    await collab.recoverUnsaved()
    await until(() => w.backend.payloads.length > 0, 5000, 'the save after the restart')
    await sleep(100)
    const [save, ...more] = w.backend.payloads
    assert.equal(save.quiet, true)
    assert.deepEqual(save.contributors, ['user-1', 'user-2'])
    assert.match(save.htmlContent, /maya@0x3[\s\S]*ana@0x5/)
    assert.deepEqual(more, [])
    await collab.stop()
    assert.deepEqual(rejections, [])
})

// A quiet write refused inside its transaction (someone typed between the
// check and the write) takes its quiet mark back before the op lets go of the
// doc, which stores it at once: that save holds only what the person typed,
// and is loud. (The save debounce is production's here, so the op's own store
// is the one save in that window.)
test('a quiet write refused in its transaction leaves no quiet mark behind', async () => {
    let typeNow = null
    const w = await world({
        body: '',
        server: { debounce: 5000, maxDebounce: 30000 },
        liveDeps: (deps) => ({
            ...deps,
            contributors: {
                ...deps.contributors,
                // Someone types between the op's check and its write.
                record: async (id, who) => {
                    await deps.contributors.record(id, who)
                    if (typeNow) await typeNow()
                },
            },
        }),
    })
    const tab = await w.tab('{"sub":"user-2"}')
    typeNow = async () => {
        const p = new Y.XmlElement('paragraph')
        p.insert(0, [new Y.XmlText('Typed while the seed waited.')])
        tab.doc.getXmlFragment('default').insert(0, [p])
        await until(() => times(w.collab.server.hocuspocus.documents.get(ID), 'Typed while the seed waited.') === 1, 2000, 'the typing on the server')
    }
    const seeded = await w.op('seed', { html: '<p>Copied.</p>', by: { user_uuid: 'user-1' }, quiet: true })
    assert.equal(seeded.status, 409, seeded.msg)
    await until(() => w.backend.saves.some((s) => s.html?.includes('Typed while the seed waited.')), 3000, 'the save of the typing')
    for (const s of w.backend.saves.filter((x) => x.html?.includes('Typed while the seed waited.'))) {
        assert.equal(s.quiet, false, "the save of someone's typing is loud")
    }
    await w.close()
    assert.deepEqual(rejections, [])
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

// A person deactivated, or taken out of the team a doc reached them through:
// every socket they have open closes, on docs and boards alike, and their
// reconnect is asked again, so the API refuses it.
test("closing people ends every socket they have open, on docs and boards, and nobody else's", async () => {
    const w = await world()
    const samDoc = await w.tab('{"sub":"sam"}')
    const priya = await w.tab('{"sub":"priya"}')
    const samBoard = await w.boardTab('{"sub":"sam"}')
    const out = await w.people('close', { user_ids: ['sam'] })
    assert.equal(out.status, 200, out.msg)
    assert.equal(out.data.closed, 2)
    await until(() => samDoc.ws.readyState === 3 && samBoard.ws.readyState === 3, 2000, "sam's sockets to close")
    assert.equal(priya.ws.readyState, 1, "priya's stays")
    w.backend.levels.set('sam', 'none')
    assert.equal(await samDoc.connect(), 'denied', 'the doc refuses sam now')
    assert.equal(await samBoard.connect(), 'denied', 'the board refuses sam now')
    // Naming nobody closes nobody: unlike reauth, this never means everyone.
    assert.equal((await w.people('close', {})).data.closed, 0)
    assert.equal((await w.people('close', { user_ids: [] })).data.closed, 0)
    assert.equal(priya.ws.readyState, 1)
    await samBoard.close()
    samBoard.destroy()
    await w.close()
})

// A guest link turned off: the guest's live view of the doc or board ends at
// once, the members' stay, and the reconnect is refused (the API re-reads the
// grant on every connect).
test("closing a guest's identity ends that guest's view of a doc or a board, and no member's", async () => {
    const w = await world()
    const member = await w.tab('{"sub":"sam"}')
    const guest = new TestClient({ url: w.collab.url, name: ID, token: '{"guest":true,"sub":"guest-g1"}' })
    assert.equal(await guest.connect(), 'synced')
    const boardGuest = await w.boardTab('{"guest":true,"sub":"guest-g2"}')
    const out = await w.people('close', { user_ids: ['guest-g1', 'guest-g2'] })
    assert.equal(out.status, 200, out.msg)
    assert.equal(out.data.closed, 2)
    await until(() => guest.ws.readyState === 3 && boardGuest.ws.readyState === 3, 2000, "the guests' sockets to close")
    assert.equal(member.ws.readyState, 1, "the member's stays")
    w.backend.levels.set('guest-g1', 'none')
    w.backend.levels.set('guest-g2', 'none')
    assert.equal(await guest.connect(), 'denied')
    assert.equal(await boardGuest.connect(), 'denied')
    for (const t of [guest, boardGuest]) {
        await t.close()
        t.destroy()
    }
    await w.close()
})

// A board's sharing changed: reauth reaches the board at its own address (its
// document is board:<uuid>), so the people named, or everyone, reconnect and
// are authorised afresh: refused when taken off it, read-only when made a
// viewer.
test('reauth reaches a board: the people named reconnect as the board now lets them, everyone when nobody is named', async () => {
    const w = await world()
    const sam = await w.boardTab('{"sub":"sam"}')
    const priya = await w.boardTab('{"sub":"priya"}')
    const out = await w.board('reauth', { user_uuids: ['sam'] })
    assert.equal(out.status, 200, out.msg)
    assert.equal(out.data.closed, 1)
    await until(() => sam.ws.readyState === 3, 2000, "sam's board socket to close")
    assert.equal(priya.ws.readyState, 1, "priya's stays")
    w.backend.levels.set('sam', 'none')
    assert.equal(await sam.connect(), 'denied', 'taken off the board')
    w.backend.levels.set('priya', 'view')
    assert.equal((await w.board('reauth', {})).data.closed, 1, 'everyone, when nobody is named')
    await until(() => priya.ws.readyState === 3, 2000, "priya's board socket to close")
    assert.equal(await priya.connect(), 'synced')
    assert.equal(priya.scope, 'readonly', 'made a viewer')
    for (const t of [sam, priya]) {
        await t.close()
        t.destroy()
    }
    await w.close()
})

// A guest link that runs out while it is open: the socket closes when it
// does, as the guest authorize answer's 'until' says, and the reconnect is
// refused.
test("a guest's socket closes when their link runs out", async () => {
    const w = await world()
    const member = await w.tab('{"sub":"sam"}')
    w.backend.until.set('guest-g1', new Date(Date.now() + 400).toISOString())
    const guest = new TestClient({ url: w.collab.url, name: ID, token: '{"guest":true,"sub":"guest-g1"}' })
    assert.equal(await guest.connect(), 'synced')
    w.backend.levels.set('guest-g1', 'none')
    await until(() => guest.ws.readyState === 3, 3000, "the guest's socket to close at the link's end")
    assert.equal(member.ws.readyState, 1, "the member's stays")
    assert.equal(await guest.connect(), 'denied')
    await guest.close()
    guest.destroy()
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
