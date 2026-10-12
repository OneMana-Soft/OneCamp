import { test } from 'node:test'
import assert from 'node:assert/strict'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { generateHTML } from '@tiptap/html'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { EVERY_BLOCK } from './everyBlock.fixture.js'
import { htmlToDoc, renderDocHTML } from './preserved.js'

// A doc holding every block the web editor writes goes through the server's
// save and load unchanged: written as HTML and read back, and through the Yjs
// document a browser edits. Before 11 Oct the save threw on the first
// `tableEmbed`, and an image's caption, id and file name were dropped.
// Same content, same attributes: key order and an empty `attrs: {}` (how Yjs
// hands marks back) aside.
const plain = (v) => {
    if (Array.isArray(v)) return v.map(plain)
    if (!v || typeof v !== 'object') return v
    const out = {}
    for (const k of Object.keys(v).sort()) {
        if (k === 'attrs' && Object.keys(v[k] || {}).length === 0) continue
        out[k] = plain(v[k])
    }
    return out
}
const read = (html) => htmlToDoc(html, COLLAB_EXTENSIONS).json

test('every block survives HTML and back, attributes included', () => {
    const html = generateHTML(EVERY_BLOCK, COLLAB_EXTENSIONS)
    const back = plain(read(html))
    EVERY_BLOCK.content.forEach((block, i) => assert.deepEqual(back.content[i], plain(block), `block ${i} (${block.type})`))
    assert.deepEqual(back, plain(EVERY_BLOCK))
})

test('every block survives the save path from a Yjs document', () => {
    const ydoc = TiptapTransformer.toYdoc(EVERY_BLOCK, 'default', COLLAB_EXTENSIONS)
    const json = TiptapTransformer.fromYdoc(ydoc).default
    const { html, preserved, degraded } = renderDocHTML(json, COLLAB_EXTENSIONS)
    assert.deepEqual([preserved, degraded], [[], 0], 'nothing needed keeping: the schema knows it all')
    assert.doesNotMatch(html, /Error generating HTML/)
    const reloaded = TiptapTransformer.toYdoc(read(html), 'default', COLLAB_EXTENSIONS)
    assert.deepEqual(plain(TiptapTransformer.fromYdoc(reloaded).default), plain(EVERY_BLOCK))
})

test('the HTML the web editor writes reads as the same blocks', () => {
    // As the web's renderHTML writes them (use-minimal-tiptap.ts, extensions/*).
    const web = [
        '<div data-type="table-embed" data-table-id="tbl-1" data-view-id="view-1"></div>',
        '<div data-type="poll" data-id="poll-1"></div>',
        '<div data-type="chart" data-spec="{&quot;type&quot;:&quot;bar&quot;}"></div>',
        '<div data-type="diff" data-diff="-x&#10;+y &lt;b&gt;"></div>',
        '<div data-type="recording-embed" data-egress="eg" data-kind="video" data-sid="s" data-u1="a" data-u2="b"></div>',
        '<p><span class="channel-mention hover:cursor-pointer" data-id="c-1" data-label="eng" data-type="channelMention">#eng</span> ' +
            '<span class="entity-mention hover:cursor-pointer" data-id="doc:1" data-label="Plan" data-type="entityMention">Plan</span></p>',
        '<img src="https://x/y.png" width="300" caption="Cap" id="abc" filename="y.png">',
    ].join('')
    const doc = plain(read(web))
    assert.deepEqual(doc.content.map((n) => n.type), ['tableEmbed', 'poll', 'chartEmbed', 'diffEmbed', 'recordingEmbed', 'paragraph', 'image'])
    assert.deepEqual(doc.content[0].attrs, { tableId: 'tbl-1', viewId: 'view-1' })
    assert.equal(doc.content[2].attrs.spec, '{"type":"bar"}')
    assert.equal(doc.content[3].attrs.diff, '-x\n+y <b>')
    assert.deepEqual(doc.content[5].content.filter((n) => n.type !== 'text').map((n) => [n.type, n.attrs.id, n.attrs.label]),
        [['channelMention', 'c-1', 'eng'], ['entityMention', 'doc:1', 'Plan']])
    assert.equal(doc.content[6].attrs.caption, 'Cap')
    assert.equal(doc.content[6].attrs.id, 'abc')
    assert.equal(doc.content[6].attrs.fileName, 'y.png')
})
