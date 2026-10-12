import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { EVERY_BLOCK } from './everyBlock.fixture.js'
import { persistDoc } from './storeDoc.js'

// The save path: a failure never replaces good content.
const quiet = () => {
    const lines = []
    const rec = (...a) => lines.push(a.map(String).join(' '))
    return { log: rec, warn: rec, error: rec, lines }
}
const docOf = (json, exts = COLLAB_EXTENSIONS) => TiptapTransformer.toYdoc(json, 'default', exts)
const ID = '7d0e3c3e-5b8a-4c9e-9a51-3f1f0b3b9a11'

test('a doc holding every block saves its whole body and its state', async () => {
    const posts = []
    const out = await persistDoc({ id: ID, document: docOf(EVERY_BLOCK), extensions: COLLAB_EXTENSIONS, post: async (p) => posts.push(p), log: quiet() })
    assert.equal(out, 'saved')
    assert.equal(posts.length, 1)
    const p = posts[0]
    for (const s of ['data-type="table-embed"', 'data-type="channelMention"', 'data-type="entityMention"', 'data-type="poll"', 'data-type="chart"', 'data-type="diff"', 'data-type="recording-embed"', 'caption="Revenue by month"']) {
        assert.ok(p.htmlContent.includes(s), s)
    }
    assert.ok(p.yjsState.length > 0)
    assert.ok(!p.keepBody)
    assert.match(p.textContent, /Launch plan/)
})

test('a block the schema lacks is kept in the body, loudly, and the doc still saves', async () => {
    const posts = []
    const log = quiet()
    const json = { type: 'doc', content: [{ type: 'paragraph', content: [{ type: 'text', text: 'Hello' }] }, { type: 'tableEmbed', attrs: { tableId: 't1', viewId: '' } }] }
    const old = COLLAB_EXTENSIONS.filter((e) => e.name !== 'tableEmbed')
    const out = await persistDoc({ id: ID, document: docOf(json), extensions: old, post: async (p) => posts.push(p), log })
    assert.equal(out, 'saved')
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
        contributors: async () => ['u-1'],
        storedBody: async () => '<p>The stored body.</p>',
    })
    assert.equal(out, 'kept-body')
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

test('an empty doc is never saved over a stored one', async () => {
    const posts = []
    const empty = { type: 'doc', content: [{ type: 'paragraph' }] }
    assert.equal(await persistDoc({ id: ID, document: docOf(empty), extensions: COLLAB_EXTENSIONS, post: async (p) => posts.push(p), log: quiet() }), 'skipped')
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
