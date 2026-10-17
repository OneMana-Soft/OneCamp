import { test } from 'node:test'
import assert from 'node:assert/strict'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { htmlToDoc, renderDocHTML } from './preserved.js'

// The blocks' layout (B1): an indent, a direction and columns go through the
// server's save and load, and through the Yjs document, unchanged; a doc
// written before them reads and saves exactly as it did; and a row left with
// one column (a collaborator's change on its way) is kept, never dropped.

// Same content and attributes; an attribute at its default (no indent, no
// direction, no id, no colour or quote size: B2's) is the same as none.
const plain = (v) => {
    if (Array.isArray(v)) return v.map(plain)
    if (!v || typeof v !== 'object') return v
    const out = {}
    for (const k of Object.keys(v).sort()) {
        if (['blockId', 'blockColor', 'quoteSize', 'dir', 'width'].includes(k) && v[k] === null) continue
        if (k === 'indent' && v[k] === 0) continue
        if (k === 'attrs' && Object.keys(plain(v[k] || {})).length === 0) continue
        out[k] = plain(v[k])
    }
    return out
}
const p = (text, attrs) => ({ type: 'paragraph', ...(attrs ? { attrs } : {}), content: [{ type: 'text', text }] })
const save = (doc) => renderDocHTML(doc, COLLAB_EXTENSIONS).html
const load = (html) => htmlToDoc(html, COLLAB_EXTENSIONS).json
const throughYjs = (doc) => TiptapTransformer.fromYdoc(TiptapTransformer.toYdoc(doc, 'default', COLLAB_EXTENSIONS)).default

const OLD = {
    type: 'doc',
    content: [
        { type: 'heading', attrs: { level: 2 }, content: [{ type: 'text', text: 'Plan' }] },
        p('A paragraph.'),
        { type: 'bulletList', content: [{ type: 'listItem', content: [p('an item')] }] },
        { type: 'blockquote', content: [p('a quote')] },
    ],
}

const LAYOUT = {
    type: 'doc',
    content: [
        p('Parent'),
        p('Child', { indent: 1 }),
        p('Grandchild, right to left', { indent: 2, dir: 'rtl' }),
        { type: 'bulletList', attrs: { indent: 1, dir: 'ltr' }, content: [{ type: 'listItem', content: [p('an item')] }] },
        {
            type: 'columnList',
            content: [
                { type: 'column', attrs: { width: 30 }, content: [p('Left')] },
                { type: 'column', attrs: { width: 70 }, content: [p('Right'), p('and more', { indent: 1 })] },
            ],
        },
    ],
}

test('a doc written before the layout reads and saves exactly as it did', () => {
    const html = save(OLD)
    assert.doesNotMatch(html, /data-indent|dir=|column/)
    assert.deepEqual(plain(load(html)), plain(OLD))
    assert.equal(save(load(html)), html)
})

test('indent, direction and columns go through the saved copy and back', () => {
    const html = save(LAYOUT)
    assert.match(html, /<p[^>]* data-indent="2" dir="rtl">Grandchild/)
    assert.match(html, /data-type="column-list"><div data-type="column" data-width="30"><p>Left/)
    assert.deepEqual(plain(load(html)), plain(LAYOUT))
})

test('and through the Yjs document a browser edits', () => {
    assert.deepEqual(plain(throughYjs(LAYOUT)), plain(LAYOUT))
})

test('a row left with one column is kept, its blocks in it', () => {
    const lone = { type: 'doc', content: [{ type: 'columnList', content: [{ type: 'column', content: [p('Only')] }] }, p('after')] }
    assert.deepEqual(plain(load(save(lone))), plain(lone))
    assert.deepEqual(plain(throughYjs(lone)), plain(lone))
})

test('what no editor writes is read as nothing: an indent past eight, a sideways direction, a width of 100', () => {
    const json = load('<p data-indent="12" dir="sideways">a</p><div data-type="column-list"><div data-type="column" data-width="100"><p>b</p></div><div data-type="column" data-width="x"><p>c</p></div></div>')
    assert.equal(json.content[0].attrs.indent, 8)
    assert.equal(json.content[0].attrs.dir, null)
    assert.deepEqual(json.content[1].content.map((c) => c.attrs.width), [null, null])
})
