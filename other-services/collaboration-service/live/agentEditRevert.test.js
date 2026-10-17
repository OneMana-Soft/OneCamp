import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { FakeBackend, FakeRedis, TestClient, onClose, startServer, until } from '../testkit.js'
import { createLiveServer } from './server.js'

// Revert of an agent's edit (docs programme B9, editor.md P2-2), run as the API
// runs it (business/AI docEdit.go): the blocks the agent wrote are replaced by
// the blocks they replaced, read from the version kept before the write, ids and
// all; an addition is taken out. The doc then reads exactly as it did before the
// agent wrote, in the live copy and in a tab that had it open throughout.

const rejections = []
const onRejection = (r) => rejections.push(r)
process.on('unhandledRejection', onRejection)
after(() => process.off('unhandledRejection', onRejection))

const ID = '7b2f4c1e-9a3d-4e6f-8b5c-1d2e3f4a5b6c'
const SECRET = 'test-secret'
const BODY =
    '<h2 data-block-id="risks">Risks</h2>' +
    '<p data-block-id="r1">Vendor <strong>delay</strong></p>' +
    '<p data-block-id="r2">Typos in the deck</p>' +
    '<h2 data-block-id="next">Next steps</h2>' +
    '<p data-block-id="n1">Ship it</p>'

const json = (doc) => JSON.stringify(TiptapTransformer.fromYdoc(doc, 'default'))

async function world() {
    const backend = new FakeBackend()
    backend.doc(ID, BODY)
    const collab = await startServer({ redis: new FakeRedis(), backend })
    const live = createLiveServer({ getSecret: () => SECRET, hocuspocus: collab.server.hocuspocus, deps: collab.live })
    await new Promise((resolve) => live.listen(0, '127.0.0.1', resolve))
    const stopLive = onClose(() => new Promise((resolve) => {
        live.close(resolve)
        live.closeAllConnections()
    }))
    const base = `http://127.0.0.1:${live.address().port}`
    const op = async (name, payload) => {
        const res = await fetch(`${base}/live/docs/${ID}/${name}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'X-Internal-Secret': SECRET },
            body: JSON.stringify(payload),
        })
        return { status: res.status, ...(await res.json()) }
    }
    const tab = new TestClient({ url: collab.url, name: ID, token: '{"sub":"sam"}' })
    assert.equal(await tab.connect(), 'synced')
    const close = async () => {
        await tab.close()
        tab.destroy()
        await stopLive()
        await collab.stop()
    }
    return { op, tab, close }
}

// The top-level blocks of a doc's HTML, by their ids, as written: what the API
// reads out of a version (ParseDocBlocks).
function blocksById(html) {
    const out = new Map()
    for (const m of html.matchAll(/<(h[1-6]|p|ul|ol|pre|blockquote)\b[^>]*data-block-id="([^"]+)"[^>]*>[\s\S]*?<\/\1>/g)) out.set(m[2], m[0])
    return out
}

const AGENT = { by: { user_uuid: 'sam', agent_uuid: 'release-captain' }, agent_edit: true, run_id: 'run-1', snapshot_first: true }

test('reverting a replaced section puts back exactly what was there, ids and all', async () => {
    const w = await world()
    const original = await w.op('read', {})
    assert.equal(original.status, 200, original.msg)
    const tabBefore = json(w.tab.doc)

    const wrote = await w.op('replaceBlocks', { block_ids: ['r1', 'r2'], html: '<ul><li><p>Vendor delay of two weeks</p></li><li><p>Venue not confirmed</p></li></ul>', ...AGENT })
    assert.equal(wrote.status, 200, wrote.msg)
    assert.ok(wrote.data.edit_id, 'the edit is marked as the agent\'s')
    assert.ok(wrote.data.before?.html, 'the doc as it was is handed over to keep as a version')
    await until(() => json(w.tab.doc).includes('Venue not confirmed'), 2000, 'the agent\'s edit in the tab')

    // Revert: the version's blocks with the replaced ids, in order, in place of
    // what the agent wrote.
    const version = blocksById(wrote.data.before.html)
    const prior = ['r1', 'r2'].map((id) => version.get(id)).join('')
    assert.match(prior, /data-block-id="r1"/)
    const reverted = await w.op('replaceBlocks', { block_ids: wrote.data.block_ids, html: prior, by: { user_uuid: 'ana' }, snapshot_first: true })
    assert.equal(reverted.status, 200, reverted.msg)

    const now = await w.op('read', {})
    assert.equal(now.data.html, original.data.html, 'the doc reads exactly as before the agent wrote')
    await until(() => json(w.tab.doc) === tabBefore, 2000, 'the open tab is back as it was')
    await w.close()
    assert.deepEqual(rejections, [])
})

test('reverting an addition takes it out and nothing else', async () => {
    const w = await world()
    const original = await w.op('read', {})
    const wrote = await w.op('insertAfter', { block_id: 'r2', html: '<p>Budget overrun</p><p>Late sign-off</p>', ...AGENT })
    assert.equal(wrote.status, 200, wrote.msg)
    assert.equal(wrote.data.block_ids.length, 2)
    const reverted = await w.op('replaceBlocks', { block_ids: wrote.data.block_ids, html: '', by: { user_uuid: 'ana' } })
    assert.equal(reverted.status, 200, reverted.msg)
    assert.equal(reverted.data.removed, 2)
    assert.equal((await w.op('read', {})).data.html, original.data.html)
    await w.close()
})

test('a revert of blocks someone deleted since is refused, never written somewhere else', async () => {
    const w = await world()
    const wrote = await w.op('append', { html: '<p>Rollback: revert the deploy.</p>', ...AGENT })
    assert.equal(wrote.status, 200, wrote.msg)
    await w.op('replaceBlocks', { block_ids: wrote.data.block_ids, html: '', by: { user_uuid: 'ana' } })
    const again = await w.op('replaceBlocks', { block_ids: wrote.data.block_ids, html: '', by: { user_uuid: 'ana' } })
    assert.equal(again.status, 404, 'the API reads the doc first and reverts only what is still there')
    await w.close()
})
