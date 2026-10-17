import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { getSchema } from '@tiptap/core'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { COLLAB_EXTENSIONS } from '../extensions.js'
import { FIELD } from '../lineage.js'
import { htmlToDoc } from '../preserved.js'
import { clientFor, createLegacyContentTransform, moveLegacyContent, needsMoving } from './legacyContent.js'

// Old content moved over as a doc loads (docs programme B2, I5): a toggle's
// title becomes its first line and its shared `open` goes; a colour becomes
// its name. Nothing a person wrote is lost, a second run changes nothing, and
// two servers moving one doc over at once never double a title.

// A doc as the old editor saved it: a toggle with its title and open in
// attributes, coloured text with the CSS variable and a paste's black.
function oldDoc() {
    const ydoc = new Y.Doc()
    ydoc.transact(() => {
        const toggle = new Y.XmlElement('collapsible')
        toggle.setAttribute('title', 'Risks & asks')
        toggle.setAttribute('open', false)
        toggle.setAttribute('blockId', 'blk-t')
        const body = new Y.XmlElement('paragraph')
        body.insert(0, [new Y.XmlText('Body')])
        toggle.insert(0, [body])
        const plain = new Y.XmlElement('collapsible')
        plain.setAttribute('title', 'Toggle')
        plain.setAttribute('open', true)
        const inside = new Y.XmlElement('paragraph')
        inside.insert(0, [new Y.XmlText('Inside')])
        plain.insert(0, [inside])
        const line = new Y.XmlElement('paragraph')
        const text = new Y.XmlText()
        text.insert(0, 'blue', { textStyle: { color: 'var(--camp-sky-ink)' } })
        // Plain text says so: Yjs gives text inserted without attributes the formatting to its left.
        text.insert(4, ' and ', {})
        text.insert(9, 'black', { textStyle: { color: 'rgb(0, 0, 0)' } })
        text.insert(14, ' and ', {})
        text.insert(19, 'named', { textStyle: { color: 'berry' }, bold: {} })
        line.insert(0, [text])
        ydoc.getXmlFragment(FIELD).insert(0, [toggle, plain, line])
    })
    return ydoc
}

// The doc's JSON with block ids, attributes at none (a block's colour, B1's
// indent 0), empty attrs and the order of marks left out.
const tidy = (v) => {
    if (Array.isArray(v)) return v.map(tidy)
    if (!v || typeof v !== 'object') return v
    const out = {}
    for (const [k, val] of Object.entries(v)) {
        if (k === 'blockId') continue
        if (k === 'attrs') {
            const attrs = Object.fromEntries(Object.entries(tidy(val)).filter(([name, a]) => a !== null && !(name === 'indent' && a === 0)))
            if (Object.keys(attrs).length) out.attrs = attrs
        } else out[k] = k === 'marks' ? tidy(val).sort((a, b) => a.type.localeCompare(b.type)) : tidy(val)
    }
    return out
}
const json = (ydoc) => TiptapTransformer.fromYdoc(ydoc, FIELD)
const p = (text, marks) => ({ type: 'paragraph', content: [marks ? { type: 'text', text, marks } : { type: 'text', text }] })

test('an old toggle opens with its title as its first line, and no shared open', () => {
    const ydoc = oldDoc()
    assert.equal(needsMoving(ydoc.getXmlFragment(FIELD)), true)
    assert.ok(moveLegacyContent(ydoc, { docName: 'd1' }) > 0)
    const doc = json(ydoc)
    assert.deepEqual(doc.content[0], { type: 'collapsible', attrs: { blockId: 'blk-t' }, content: [p('Risks & asks'), p('Body')] })
    assert.deepEqual(doc.content[1], { type: 'collapsible', content: [p('Toggle'), p('Inside')] })
    // The new schema holds it as it is.
    assert.doesNotThrow(() => getSchema(COLLAB_EXTENSIONS).nodeFromJSON(doc).check())
})

test('an old toggle with no title gets an empty first line, so its blocks stay its blocks', () => {
    const ydoc = new Y.Doc()
    ydoc.transact(() => {
        const toggle = new Y.XmlElement('collapsible')
        toggle.setAttribute('open', true)
        const body = new Y.XmlElement('paragraph')
        body.insert(0, [new Y.XmlText('Still the body')])
        toggle.insert(0, [body])
        ydoc.getXmlFragment(FIELD).insert(0, [toggle])
    })
    assert.equal(moveLegacyContent(ydoc, { docName: 'd3' }), 1)
    assert.deepEqual(tidy(json(ydoc).content[0]), { type: 'collapsible', content: [{ type: 'paragraph' }, p('Still the body')] })
    assert.equal(moveLegacyContent(ydoc, { docName: 'd3' }), 0)
})

test('an old colour becomes its name, and a colour that is not ours goes', () => {
    const ydoc = oldDoc()
    moveLegacyContent(ydoc, { docName: 'd1' })
    assert.deepEqual(tidy(json(ydoc).content[2].content), [
        { type: 'text', text: 'blue', marks: [{ type: 'textStyle', attrs: { color: 'sky' } }] },
        { type: 'text', text: ' and black and ' },
        { type: 'text', text: 'named', marks: [{ type: 'bold' }, { type: 'textStyle', attrs: { color: 'berry' } }] },
    ])
})

test('a second run finds nothing, and a doc with nothing old is not touched', () => {
    const ydoc = oldDoc()
    moveLegacyContent(ydoc, { docName: 'd1' })
    let updates = 0
    ydoc.on('update', () => updates++)
    assert.equal(moveLegacyContent(ydoc, { docName: 'd1' }), 0)
    const fresh = TiptapTransformer.toYdoc({ type: 'doc', content: [{ type: 'collapsible', content: [p('New'), p('kind')] }] }, FIELD, COLLAB_EXTENSIONS)
    fresh.on('update', () => updates++)
    assert.equal(moveLegacyContent(fresh, { docName: 'd2' }), 0)
    assert.equal(updates, 0)
})

test('two servers moving one doc over at once write one title, not two', () => {
    const a = oldDoc()
    const b = new Y.Doc()
    Y.applyUpdate(b, Y.encodeStateAsUpdate(a))
    moveLegacyContent(a, { docName: 'd1' })
    moveLegacyContent(b, { docName: 'd1' })
    Y.applyUpdate(a, Y.encodeStateAsUpdate(b))
    Y.applyUpdate(b, Y.encodeStateAsUpdate(a))
    assert.deepEqual(json(a), json(b))
    assert.deepEqual(json(a).content[0].content.map((n) => n.content?.[0]?.text), ['Risks & asks', 'Body'])
    assert.notEqual(clientFor('d1', ''), clientFor('d2', ''))
})

test('runs as the server, never counted as an editor, and says what it did', () => {
    const lines = []
    const transform = createLegacyContentTransform({ log: { log: (l) => lines.push(l) } })
    const ydoc = oldDoc()
    const origins = []
    ydoc.on('update', (_u, origin) => origins.push(origin))
    assert.ok(transform.give('d1', ydoc) > 0)
    assert.deepEqual(origins, [transform.origin])
    assert.equal(transform.origin.context.user.id, 'unknown')
    assert.match(lines[0], /d1: moved \d+ old toggles? and colours over/)
})

test('an old body read from HTML has the same new shapes', () => {
    const html =
        '<div data-type="collapsible" data-title="Risks &amp; asks" data-open="false"><p>Body</p></div>' +
        '<p><span style="color: var(--camp-sky-ink)">blue</span> and <span style="color: rgb(0, 0, 0)">black</span></p>' +
        '<details><summary>From <strong>Notion</strong></summary><p>Pasted</p></details>'
    const doc = htmlToDoc(html, COLLAB_EXTENSIONS).json
    assert.deepEqual(tidy(doc.content[0]), { type: 'collapsible', content: [p('Risks & asks'), p('Body')] })
    assert.deepEqual(tidy(doc.content[1]).content, [
        { type: 'text', text: 'blue', marks: [{ type: 'textStyle', attrs: { color: 'sky' } }] },
        { type: 'text', text: ' and black' },
    ])
    assert.equal(doc.content[2].type, 'collapsible')
    assert.equal(doc.content[2].content[0].content.map((t) => t.text).join(''), 'From Notion')
})

test('saved HTML reads as the codec reads it: no title is an empty first line, a heading summary a toggle heading', () => {
    const html =
        '<div data-type="collapsible" data-open="true"><p>Body</p></div>' +
        '<details><summary><h2>Release <em>notes</em></h2></summary><p>Shipped</p></details>' +
        '<details><p>No summary</p></details>' +
        '<div data-type="collapsible"><p>New</p><p>kind</p></div>'
    const doc = htmlToDoc(html, COLLAB_EXTENSIONS).json
    assert.deepEqual(tidy(doc.content[0]), { type: 'collapsible', content: [{ type: 'paragraph' }, p('Body')] })
    assert.deepEqual(tidy(doc.content[1]), {
        type: 'collapsible',
        content: [
            { type: 'heading', attrs: { level: 2 }, content: [{ type: 'text', text: 'Release ' }, { type: 'text', text: 'notes', marks: [{ type: 'italic' }] }] },
            p('Shipped'),
        ],
    })
    assert.deepEqual(tidy(doc.content[2]), { type: 'collapsible', content: [{ type: 'paragraph' }, p('No summary')] })
    assert.deepEqual(tidy(doc.content[3]), { type: 'collapsible', content: [p('New'), p('kind')] })
})
