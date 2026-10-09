import { test } from 'node:test'
import assert from 'node:assert/strict'
import * as Y from 'yjs'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { generateHTML, generateJSON } from '@tiptap/html'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { bodyHash, encodeState, savedStateFor } from './docState.js'

// As the server does it: the stored body converted into a fresh document.
const fromBody = (html) => TiptapTransformer.toYdoc(generateJSON(html, COLLAB_EXTENSIONS), 'default', COLLAB_EXTENSIONS)
// As the server saves it (with the xmlns generateHTML adds on the server).
const bodyOf = (ydoc) => generateHTML(TiptapTransformer.fromYdoc(ydoc, 'default'), COLLAB_EXTENSIONS)
// A browser and the server exchanging what each lacks, as a provider does on connect.
const sync = (a, b) => {
    Y.applyUpdate(a, Y.encodeStateAsUpdate(b))
    Y.applyUpdate(b, Y.encodeStateAsUpdate(a))
}
const times = (html, text) => html.split(text).length - 1

const body = '<h1>Launch plan</h1><p>Ship on Thursday.</p>'

test('a browser that kept its copy meets a document rebuilt from HTML, and it doubles (the bug)', () => {
    const server = fromBody(body)
    const browser = new Y.Doc()
    sync(browser, server) // the browser opens it
    const reloaded = fromBody(bodyOf(server)) // unloaded, then rebuilt from the saved body
    sync(browser, reloaded) // the browser reconnects with its copy
    assert.equal(times(bodyOf(reloaded), 'Ship on Thursday.'), 2)
})

test('opened from the state it was saved with, the same browser syncs to one copy', () => {
    const server = fromBody(body)
    const browser = new Y.Doc()
    sync(browser, server)
    const saved = bodyOf(server)
    const reloaded = savedStateFor(saved, encodeState(server), bodyHash(saved))
    assert.ok(reloaded, 'the saved state matches the saved body')
    sync(browser, reloaded)
    assert.equal(times(bodyOf(reloaded), 'Ship on Thursday.'), 1)
    assert.equal(times(bodyOf(browser), 'Ship on Thursday.'), 1)
})

test('edits made while the document was away still arrive, once', () => {
    const server = fromBody(body)
    const browser = new Y.Doc()
    sync(browser, server)
    const saved = bodyOf(server)
    const state = encodeState(server)
    // Offline, the browser adds a line.
    const xml = browser.getXmlFragment('default')
    const p = new Y.XmlElement('paragraph')
    p.insert(0, [new Y.XmlText('Added while offline.')])
    xml.insert(xml.length, [p])
    const reloaded = savedStateFor(saved, state, bodyHash(saved))
    sync(browser, reloaded)
    const html = bodyOf(reloaded)
    assert.equal(times(html, 'Ship on Thursday.'), 1)
    assert.equal(times(html, 'Added while offline.'), 1)
})

test('a body changed another way is rebuilt, not opened from a stale state', () => {
    const server = fromBody(body)
    const saved = bodyOf(server)
    const restored = '<p>An older version, restored.</p>'
    assert.equal(savedStateFor(restored, encodeState(server), bodyHash(saved)), null)
})

test('no saved state, or one that is not a document, is rebuilt', () => {
    const server = fromBody(body)
    const saved = bodyOf(server)
    assert.equal(savedStateFor(saved, '', bodyHash(saved)), null)
    assert.equal(savedStateFor(saved, undefined, undefined), null)
    assert.equal(savedStateFor(saved, encodeState(server), ''), null)
    assert.equal(savedStateFor(saved, Buffer.from('not a yjs update').toString('base64'), bodyHash(saved)), null)
    assert.equal(savedStateFor(saved, encodeState(new Y.Doc()), bodyHash(saved)), null, 'an empty state never replaces a body')
})

test('the body hash agrees with the backend (business/Doc collabBodyHash pins the same value)', () => {
    assert.equal(bodyHash('<p>Ship on Thursday. Café ✓</p>'), 'aa0219c1b9ca040a1d19798de36fcb4ff57ccfc02209bcf492e615a1cd72c7af')
})
