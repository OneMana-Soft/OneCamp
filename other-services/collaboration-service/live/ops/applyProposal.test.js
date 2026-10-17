import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { getSchema } from '@tiptap/core'
import { initProseMirrorDoc } from 'y-prosemirror'
import { COLLAB_EXTENSIONS } from '../../extensions.js'
import { FakeBackend, FakeRedis, TestClient, startServer, until } from '../../testkit.js'
import { createLiveServer } from '../server.js'
import { applyProposal } from './applyProposal.js'
import { diffBlocks, encodePlaces, encodeTarget, siblingsBetween } from './proposalCore.js'

// G-COLLAB for edit proposals: the op against a live doc on a real collaboration
// server, with a browser-like client connected over a websocket, as a reviewer
// accepting from a list would meet it.

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

const DOC = '5b0c2a5e-1d7a-4f0e-9a1c-3c8e2d4f6a70'
const PID = '7c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f'
const schema = getSchema(COLLAB_EXTENSIONS)
const p = (text) => ({ type: 'paragraph', content: [{ type: 'text', text }] })

const pmOf = (ydoc) => initProseMirrorDoc(ydoc.getXmlFragment('default'), schema)
const textOf = (ydoc) => {
    const { doc } = pmOf(ydoc)
    return doc.textBetween(0, doc.content.size, '|')
}

async function world(body) {
    const redis = new FakeRedis()
    const backend = new FakeBackend()
    backend.doc(DOC, body)
    const collab = await startServer({ redis, backend })
    const tab = new TestClient({ url: collab.url, name: DOC })
    assert.equal(await tab.connect(), 'synced')
    return { backend, collab, tab, stop: async () => { await tab.close(); tab.destroy(); await collab.stop() } }
}

// A proposal as a client makes it: against the tab's own copy of the doc.
function proposeFrom(ydoc, targetIndex, proposedJSON) {
    const { doc } = pmOf(ydoc)
    const fragment = ydoc.getXmlFragment('default')
    let from = 0
    for (let i = 0; i < targetIndex; i++) from += doc.child(i).nodeSize
    const to = from + doc.child(targetIndex).nodeSize
    const base = siblingsBetween(doc, from, to)
    const proposed = proposedJSON.map((n) => schema.nodeFromJSON(n))
    return {
        id: PID,
        resource_kind: 'doc',
        resource_uuid: DOC,
        anchor: { kind: 'yrange', ...encodeTarget(fragment, doc, { from, to }) },
        base: base.map((b) => b.toJSON()),
        proposed: proposedJSON,
        hunks: encodePlaces(fragment, doc, from, base, diffBlocks(base, proposed)),
    }
}

const run = (collab, proposal, mode, ids = []) => applyProposal({
    docName: DOC,
    body: { proposal_id: PID, hunk_ids: ids, by: { user_uuid: 'reviewer-1' } },
    hocuspocus: collab.server.hocuspocus,
    loadProposal: async () => ({ ...proposal, pending: { mode, hunk_ids: ids } }),
})

test('accepted on the server, the change reaches the open tab, after the tab moved the target', async () => {
    const w = await world('<p>Intro.</p><p>We ship on Thursday and tell sales.</p><p>End.</p>')
    try {
        const prop = proposeFrom(w.tab.doc, 1, [p('We ship on Friday and tell everyone.')])
        assert.equal(prop.hunks.length, 2)
        // The tab edits above the target, and the server has it before the accept.
        w.tab.doc.getXmlFragment('default').get(0).get(0).insert(0, 'A longer ')
        await until(async () => textOf(w.collab.server.hocuspocus.documents.get(DOC)).startsWith('A longer'), 3000, 'the edit above')
        const out = await run(w.collab, prop, 'apply', ['h1', 'h2'])
        assert.deepEqual(out.applied, ['h1', 'h2'])
        await until(() => textOf(w.tab.doc) === 'A longer Intro.|We ship on Friday and tell everyone.|End.', 3000, 'the tab to see the accept')
    } finally {
        await w.stop()
    }
})

test('one hunk of two: only it lands; a hunk whose words changed is a 409 and nothing is written', async () => {
    const w = await world('<p>We ship on Thursday and tell sales.</p>')
    try {
        const prop = proposeFrom(w.tab.doc, 0, [p('We ship on Friday and tell everyone.')])
        const out = await run(w.collab, prop, 'apply', ['h1'])
        assert.deepEqual(out.applied, ['h1'])
        await until(() => textOf(w.tab.doc) === 'We ship on Friday and tell sales.', 3000, 'the first hunk')
        // The tab rewrites the second hunk's own word.
        const text = w.tab.doc.getXmlFragment('default').get(0).get(0)
        const at = text.toString().indexOf('sales')
        w.tab.doc.transact(() => {
            text.delete(at, 5)
            text.insert(at, 'marketing')
        })
        const live = w.collab.server.hocuspocus.documents.get(DOC)
        await until(() => textOf(live).includes('marketing'), 3000, 'the edit inside the hunk')
        const before = Y.encodeStateVector(live)
        await assert.rejects(run(w.collab, prop, 'apply', ['h2']), (e) => e.status === 409 && JSON.parse(e.message).conflicts[0] === 'h2')
        assert.deepEqual(Y.encodeStateVector(live), before)
    } finally {
        await w.stop()
    }
})

test('a deleted target: apply is refused, Insert below puts the proposal where it was', async () => {
    const w = await world('<p>Before.</p><p>Target text.</p><p>After.</p>')
    try {
        const prop = proposeFrom(w.tab.doc, 1, [p('Better text.')])
        w.tab.doc.getXmlFragment('default').delete(1, 1)
        const live = w.collab.server.hocuspocus.documents.get(DOC)
        await until(() => textOf(live) === 'Before.|After.', 3000, 'the delete')
        await assert.rejects(run(w.collab, prop, 'apply', ['h1']), (e) => e.status === 409)
        await run(w.collab, prop, 'insert_below')
        await until(() => textOf(w.tab.doc) === 'Before.|Better text.|After.', 3000, 'the insert below')
    } finally {
        await w.stop()
    }
})

test('prepare: an agent proposal made from HTML gets its hunks against the live doc, and nothing is written', async () => {
    const w = await world('<h2>Launch</h2><p>We ship on Thursday.</p><h2>Risks</h2><p>None yet.</p>')
    try {
        let prepared = null
        const live = w.collab.server.hocuspocus.documents.get(DOC)
        const before = Y.encodeStateVector(live)
        const out = await applyProposal({
            docName: DOC,
            body: { proposal_id: PID, hunk_ids: [] },
            hocuspocus: w.collab.server.hocuspocus,
            loadProposal: async () => ({ id: PID, resource_uuid: DOC, pending: { mode: 'prepare', target: { section: 'launch' }, html: '<p>We ship on Friday.</p><p>Then we celebrate.</p>' } }),
            savePrepared: async (id, data) => { prepared = data },
        })
        assert.equal(out.prepared, true)
        assert.deepEqual(Y.encodeStateVector(live), before)
        assert.equal(prepared.anchor.kind, 'yrange')
        assert.match(prepared.base_hash, /^[0-9a-f]{64}$/)
        assert.deepEqual(prepared.hunks.map((h) => [h.level, h.op]), [['inline', 'replace'], ['block', 'insert']])
        // Accepting what was prepared lands under the heading, in the open tab.
        await run(w.collab, { id: PID, resource_uuid: DOC, ...prepared }, 'apply', ['h1', 'h2'])
        await until(() => textOf(w.tab.doc) === 'Launch|We ship on Friday.|Then we celebrate.|Risks|None yet.', 3000, 'the accept')
    } finally {
        await w.stop()
    }
})

test('over the internal port: the op answers, and a proposal for another doc is refused', async () => {
    const w = await world('<p>We ship on Thursday.</p>')
    const prop = proposeFrom(w.tab.doc, 0, [p('We ship on Friday.')])
    const secret = 'a-test-secret-long-enough-to-count-0123456789'
    const routes = new Map([['applyProposal', (args) => applyProposal({ ...args, loadProposal: async () => ({ ...prop, pending: { mode: 'apply', hunk_ids: ['h1'] } }) })]])
    // deps: A4's live server hands its ops the collaboration server's `live`
    // (contributors, persistence); with A4 merged, this runs through its runWrite.
    const server = createLiveServer({ getSecret: () => secret, routes, hocuspocus: w.collab.server.hocuspocus, deps: w.collab.live })
    await new Promise((r) => server.listen(0, '127.0.0.1', r))
    const call = (doc) => fetch(`http://127.0.0.1:${server.address().port}/live/docs/${doc}/applyProposal`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Internal-Secret': secret },
        body: JSON.stringify({ proposal_id: PID, hunk_ids: ['h1'], by: { user_uuid: 'reviewer-1' } }),
    })
    try {
        assert.equal((await call('00000000-0000-4000-8000-000000000000')).status, 409)
        const ok = await call(DOC)
        assert.equal(ok.status, 200)
        assert.deepEqual((await ok.json()).data.applied, ['h1'])
        await until(() => textOf(w.tab.doc) === 'We ship on Friday.', 3000, 'the accept')
    } finally {
        await new Promise((r) => server.close(r))
        await w.stop()
    }
    assert.deepEqual(rejections, [])
})

// A4's runWrite (live/ops/write.js on its branch), reduced to its contract:
// locate on the doc as it is, then, inside one transaction, locate again, keep
// the body as it was (snapshot_first), and apply in an inner transaction. The
// op hands its apply and insert below to it when the live server has it.
function standInRunWrite(log) {
    return async ({ docName, body, hocuspocus }, { build, locate, apply }) => {
        const built = build()
        const connection = await hocuspocus.openDirectConnection(docName, { user: { id: body.by?.user_uuid || 'unknown' } })
        let result = null
        let before = null
        try {
            locate(connection.document, built)
            log.push('located')
            await connection.transact((doc) => {
                const at = locate(doc, built)
                if (body.snapshot_first) before = textOf(doc)
                doc.transact(() => { result = apply(doc, built, at) }, { liveDocWriter: true })
            })
        } finally {
            await connection.disconnect()
        }
        return { blocks: result.blocks, from: result.from, to: result.to, before }
    }
}

test("through A4's write contract: the step's parts locate twice, keep the body before, and apply once", async () => {
    const w = await world('<p>Intro.</p><p>We ship on Thursday.</p><p>End.</p>')
    try {
        const prop = proposeFrom(w.tab.doc, 1, [p('We ship on Friday.')])
        const log = []
        const out = await applyProposal({
            docName: DOC,
            body: { proposal_id: PID, hunk_ids: ['h1'], by: { user_uuid: 'reviewer-1' }, snapshot_first: true },
            hocuspocus: w.collab.server.hocuspocus,
            loadProposal: async () => ({ ...prop, pending: { mode: 'apply', hunk_ids: ['h1'] } }),
            runWrite: standInRunWrite(log),
        })
        assert.deepEqual(log, ['located'])
        assert.equal(out.before, 'Intro.|We ship on Thursday.|End.')
        assert.deepEqual([out.from, out.to, out.blocks], [1, 2, 1])
        await until(() => textOf(w.tab.doc) === 'Intro.|We ship on Friday.|End.', 3000, 'the accept')
        // A conflict refuses before the write: the doc is untouched.
        const live = w.collab.server.hocuspocus.documents.get(DOC)
        const sv = Y.encodeStateVector(live)
        await assert.rejects(applyProposal({
            docName: DOC,
            body: { proposal_id: PID, hunk_ids: ['h1'], by: { user_uuid: 'reviewer-1' } },
            hocuspocus: w.collab.server.hocuspocus,
            loadProposal: async () => ({ ...prop, pending: { mode: 'apply', hunk_ids: ['h1'] } }),
            runWrite: standInRunWrite([]),
        }), (e) => e.status === 409)
        assert.deepEqual(Y.encodeStateVector(live), sv)
    } finally {
        await w.stop()
    }
})
