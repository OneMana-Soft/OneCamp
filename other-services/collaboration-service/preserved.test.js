import { test } from 'node:test'
import assert from 'node:assert/strict'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { generateHTML } from '@tiptap/html'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { RenderError, decodeAttributeRefs, htmlToDoc, renderDocHTML } from './preserved.js'

// A schema that lags the editor must never cost a doc its content: what it
// does not know is written as it is, and read back as the same nodes.
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
const without = (...names) => COLLAB_EXTENSIONS.filter((e) => !names.includes(e.name))

// Blocks and marks no schema here knows, beside some an older server lacked.
const DOC = {
    type: 'doc',
    content: [
        { type: 'paragraph', content: [{ type: 'text', text: 'Before.' }] },
        { type: 'futureEmbed', attrs: { url: 'https://x/y', height: 240, opts: { a: [1, 2] } } },
        { type: 'toggleHeading', attrs: { level: 2 }, content: [{ type: 'text', text: 'A toggle heading' }] },
        { type: 'columns', content: [{ type: 'paragraph', content: [{ type: 'text', text: 'Left column' }] }] },
        {
            type: 'paragraph',
            content: [
                { type: 'text', text: 'Due ' },
                { type: 'dateMention', attrs: { date: '2026-10-12' } },
                { type: 'text', text: ' marked', marks: [{ type: 'bold' }, { type: 'highlight', attrs: { color: 'yellow' } }] },
                { type: 'text', text: ' and ' },
                { type: 'entityMention', attrs: { id: 'doc:1', label: 'Plan', mentionSuggestionChar: '+' } },
            ],
        },
        { type: 'tableEmbed', attrs: { tableId: 't1', viewId: '' } },
        { type: 'paragraph', content: [{ type: 'text', text: 'After.' }] },
    ],
}
const OLD = without('tableEmbed', 'entityMention')

test('a doc holding unknown blocks and marks is still written, every one kept', () => {
    assert.throws(() => generateHTML(DOC, OLD), /Unknown node type|no mark type/i, 'the plain write fails (the bug)')
    const { html, preserved, degraded } = renderDocHTML(DOC, OLD)
    assert.deepEqual(preserved.sort(), ['columns', 'dateMention', 'entityMention', 'futureEmbed', 'highlight', 'tableEmbed', 'toggleHeading'])
    assert.equal(degraded, 0)
    for (const text of ['Before.', 'A toggle heading', 'Left column', 'Due ', ' marked', 'After.']) assert.ok(html.includes(text), text)
    assert.doesNotMatch(html, /Error generating HTML/)
})

test('read back, the kept blocks and marks are the same nodes, under their own names', () => {
    const { html } = renderDocHTML(DOC, OLD)
    const { json, extensions } = htmlToDoc(html, OLD)
    assert.deepEqual(plain(json), plain(DOC))
    // ...and become a Yjs document a browser that knows them can edit.
    const ydoc = TiptapTransformer.toYdoc(json, 'default', extensions)
    assert.deepEqual(plain(TiptapTransformer.fromYdoc(ydoc).default), plain(DOC))
})

test('a block an older server kept is read as that block once the schema knows it', () => {
    const { html } = renderDocHTML(DOC, OLD)
    const { json } = htmlToDoc(html, COLLAB_EXTENSIONS)
    assert.deepEqual(json.content[5], { type: 'tableEmbed', attrs: { tableId: 't1', viewId: '' } })
    assert.deepEqual(plain(json.content[4].content[4]), plain(DOC.content[4].content[4]))
})

test('a block that still cannot be written is kept as its text, and the rest as it is', () => {
    const doc = { type: 'doc', content: [DOC.content[0], { type: 'callout', attrs: { emoji: '!', color: 'red' }, content: [{ type: 'paragraph', content: [{ type: 'text', text: 'Inside the callout' }] }] }, DOC.content[6]] }
    const render = (json, exts) => {
        if (JSON.stringify(json).includes('"callout"')) throw new Error('boom')
        return generateHTML(json, exts)
    }
    const { html, degraded } = renderDocHTML(doc, COLLAB_EXTENSIONS, render)
    assert.equal(degraded, 1)
    assert.match(html, /<p[^>]*>Before\.<\/p>/)
    assert.match(html, /data-oc-unrendered="callout"[^>]*>Inside the callout</)
    assert.match(html, /<p[^>]*>After\.<\/p>/)
})

test('when nothing can be written it says so, and never returns a placeholder', () => {
    assert.throws(() => renderDocHTML(DOC, COLLAB_EXTENSIONS, () => { throw new Error('boom') }), RenderError)
})

test('attribute values come back with their line breaks and angle brackets', () => {
    assert.equal(decodeAttributeRefs('<div data-diff="a&#xA;b &lt;c&gt; &amp;lt; &#34;q&#x22;">x &lt; y</div>'),
        '<div data-diff="a\nb <c> &amp;lt; &quot;q&quot;">x &lt; y</div>')
    const diff = '--- a\n+++ b\n-<old>\n+"new" & done\t&lt;'
    const html = generateHTML({ type: 'doc', content: [{ type: 'diffEmbed', attrs: { diff } }] }, COLLAB_EXTENSIONS)
    assert.equal(htmlToDoc(html, COLLAB_EXTENSIONS).json.content[0].attrs.diff, diff)
})
