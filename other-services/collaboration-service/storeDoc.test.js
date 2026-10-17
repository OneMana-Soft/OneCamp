import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { EVERY_BLOCK } from './everyBlock.fixture.js'
import { emptiedByEdit, persistDoc } from './storeDoc.js'

// The save path: a failure never replaces good content.
const quiet = () => {
    const lines = []
    const rec = (...a) => lines.push(a.map(String).join(' '))
    return { log: rec, warn: rec, error: rec, lines }
}
const docOf = (json, exts = COLLAB_EXTENSIONS) => TiptapTransformer.toYdoc(json, 'default', exts)
const ID = '7d0e3c3e-5b8a-4c9e-9a51-3f1f0b3b9a11'
const members = (list) => ({ peek: async () => list, ack: async () => {} })

test('a doc holding every block saves its whole body and its state', async () => {
    const posts = []
    const out = await persistDoc({ id: ID, document: docOf(EVERY_BLOCK), extensions: COLLAB_EXTENSIONS, post: async (p) => posts.push(p), log: quiet() })
    assert.equal(out.outcome, 'saved')
    assert.ok(out.hash && out.stateVector)
    assert.equal(posts.length, 1)
    const p = posts[0]
    // Only what the API stores: no JSON copy and no text copy of the body.
    assert.deepEqual(Object.keys(p).sort(), ['agents', 'contributors', 'documentId', 'htmlContent', 'lineage', 'yjsState'])
    for (const s of ['data-type="table-embed"', 'data-type="channelMention"', 'data-type="entityMention"', 'data-type="poll"', 'data-type="chart"', 'data-type="diff"', 'data-type="recording-embed"', 'caption="Revenue by month"']) {
        assert.ok(p.htmlContent.includes(s), s)
    }
    assert.ok(p.yjsState.length > 0)
    assert.ok(!p.keepBody)
})

test('a block the schema lacks is kept in the body, loudly, and the doc still saves', async () => {
    const posts = []
    const log = quiet()
    const json = { type: 'doc', content: [{ type: 'paragraph', content: [{ type: 'text', text: 'Hello' }] }, { type: 'tableEmbed', attrs: { tableId: 't1', viewId: '' } }] }
    const old = COLLAB_EXTENSIONS.filter((e) => e.name !== 'tableEmbed')
    const out = await persistDoc({ id: ID, document: docOf(json), extensions: old, post: async (p) => posts.push(p), log })
    assert.equal(out.outcome, 'saved')
    assert.match(posts[0].htmlContent, /<p[^>]*>Hello<\/p>/)
    assert.match(posts[0].htmlContent, /data-oc-preserved="tableEmbed"/)
    assert.ok(log.lines.some((l) => l.includes(ID) && l.includes('tableEmbed')), 'logged with the doc id')
})

test('when the body cannot be written at all, the stored body is kept and only the state is saved', async () => {
    const posts = []
    const log = quiet()
    const out = await persistDoc({
        id: ID, document: docOf(EVERY_BLOCK), extensions: COLLAB_EXTENSIONS, log,
        render: () => { throw new Error('boom') },
        post: async (p) => posts.push(p),
        contributors: members(['u-1']),
        storedBody: async () => '<p>The stored body.</p>',
    })
    assert.equal(out.outcome, 'kept-body')
    assert.equal(posts.length, 1)
    assert.equal(posts[0].keepBody, true)
    // The stored body, unchanged: an API that predates keepBody writes it back
    // instead of an empty body.
    assert.equal(posts[0].htmlContent, '<p>The stored body.</p>')
    assert.ok(posts[0].yjsState.length > 0)
    assert.deepEqual(posts[0].contributors, ['u-1'])
    assert.ok(log.lines.some((l) => l.includes(ID) && l.includes('KEPT')))
    // The state saved is the whole doc.
    const back = new Y.Doc()
    Y.applyUpdate(back, Buffer.from(posts[0].yjsState, 'base64'))
    assert.equal(back.getXmlFragment('default').length, EVERY_BLOCK.content.length)
})

test('no request ever carries the old placeholder', async () => {
    const posts = []
    for (const render of [undefined, () => { throw new Error('boom') }]) {
        await persistDoc({ id: ID, document: docOf(EVERY_BLOCK), extensions: COLLAB_EXTENSIONS, render, post: async (p) => posts.push(p), log: quiet(), storedBody: async () => '<p>Stored.</p>' })
    }
    assert.ok(posts.every((p) => !JSON.stringify(p).includes('Error generating HTML')))
})

test('a save the backend refuses throws, so the doc stays in memory and is tried again', async () => {
    await assert.rejects(persistDoc({ id: ID, document: docOf(EVERY_BLOCK), extensions: COLLAB_EXTENSIONS, post: async () => { throw new Error('502') }, log: quiet() }), /502/)
})

test('a doc that never held anything is not saved', async () => {
    const posts = []
    const empty = { type: 'doc', content: [{ type: 'paragraph' }] }
    const doc = docOf(empty)
    assert.equal(emptiedByEdit(doc), false)
    assert.equal((await persistDoc({ id: ID, document: doc, extensions: COLLAB_EXTENSIONS, post: async (p) => posts.push(p), log: quiet() })).outcome, 'skipped')
    assert.equal(posts.length, 0)
})

test('without the stored body, a keep-body save sends nothing and is retried', async () => {
    const posts = []
    await assert.rejects(persistDoc({
        id: ID, document: docOf(EVERY_BLOCK), extensions: COLLAB_EXTENSIONS, log: quiet(),
        render: () => { throw new Error('boom') },
        post: async (p) => posts.push(p),
        storedBody: async () => { throw new Error('API down') },
    }))
    assert.equal(posts.length, 0)
})

test('a doc someone emptied is saved empty, so it stays empty', async () => {
    const posts = []
    const doc = docOf({ type: 'doc', content: [{ type: 'paragraph', content: [{ type: 'text', text: 'Delete me' }] }, { type: 'paragraph', content: [{ type: 'text', text: 'And me' }] }] })
    // What select-all and Delete leaves: one empty paragraph.
    const body = doc.getXmlFragment('default')
    doc.transact(() => {
        body.delete(1, 1)
        const text = body.get(0).get(0)
        text.delete(0, text.length)
    })
    assert.equal(emptiedByEdit(doc), true)
    const out = await persistDoc({ id: ID, document: doc, extensions: COLLAB_EXTENSIONS, post: async (p) => posts.push(p), log: quiet() })
    assert.equal(out.outcome, 'saved')
    assert.equal(posts.length, 1)
    assert.doesNotMatch(posts[0].htmlContent, /Delete me|And me/)
    assert.match(posts[0].htmlContent, /<p[^>]*><\/p>/)
})

test('hash first: an unchanged body sends only the state, and an unchanged state sends nothing', async () => {
    const posts = []
    const doc = docOf(EVERY_BLOCK)
    const post = async (p) => { posts.push(p); return { stored: true } }
    const first = await persistDoc({ id: ID, document: doc, extensions: COLLAB_EXTENSIONS, post, log: quiet() })
    assert.equal(first.outcome, 'saved')

    // Nothing moved: no request at all.
    const again = await persistDoc({ id: ID, document: doc, extensions: COLLAB_EXTENSIONS, post, last: first, log: quiet() })
    assert.equal(again.outcome, 'unchanged')
    assert.equal(posts.length, 1)

    // The state moved but the body reads the same (an edit undone): a
    // state-only save, against the body's hash. It carries that same body, for
    // an API from before keepBody, which writes it back instead of an empty one.
    const p = doc.getXmlFragment('default')
    doc.transact(() => { const el = new Y.XmlElement('paragraph'); p.insert(p.length, [el]) })
    doc.transact(() => p.delete(p.length - 1, 1))
    const stateOnly = await persistDoc({ id: ID, document: doc, extensions: COLLAB_EXTENSIONS, post, last: first, log: quiet() })
    assert.equal(stateOnly.outcome, 'state-only')
    assert.equal(posts.length, 2)
    assert.equal(posts[1].keepBody, true)
    assert.equal(posts[1].expectBodyHash, first.hash)
    assert.equal(posts[1].htmlContent, posts[0].htmlContent)

    // The API says the stored body is another one: the whole body goes.
    const refusing = async (q) => { posts.push(q); return q.keepBody ? { stored: false } : { stored: true } }
    doc.transact(() => p.insert(p.length, [new Y.XmlElement('paragraph')]))
    doc.transact(() => p.delete(p.length - 1, 1))
    const whole = await persistDoc({ id: ID, document: doc, extensions: COLLAB_EXTENSIONS, post: refusing, last: stateOnly, log: quiet() })
    assert.equal(whole.outcome, 'saved')
    assert.ok(posts[posts.length - 1].htmlContent)
})

test('who edited is cleared only once the API has the save', async () => {
    const acked = []
    const contributors = { peek: async () => ['u-1', JSON.stringify({ agent: 'a-1', asker: 'u-2' })], ack: async (m) => acked.push(...m) }
    const posts = []
    await assert.rejects(persistDoc({ id: ID, document: docOf(EVERY_BLOCK), extensions: COLLAB_EXTENSIONS, contributors, post: async () => { throw new Error('503') }, log: quiet() }))
    assert.deepEqual(acked, [], 'a failed save keeps them for the next')
    await persistDoc({ id: ID, document: docOf(EVERY_BLOCK), extensions: COLLAB_EXTENSIONS, contributors, post: async (p) => posts.push(p), log: quiet() })
    assert.deepEqual(posts[0].contributors, ['u-1'])
    assert.deepEqual(posts[0].agents, [{ agent_uuid: 'a-1', asker_uuid: 'u-2' }])
    assert.equal(acked.length, 2)
})
