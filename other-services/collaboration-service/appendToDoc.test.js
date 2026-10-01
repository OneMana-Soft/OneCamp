import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Readable } from 'node:stream'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { generateHTML, generateJSON } from '@tiptap/html'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { internalSecretOK, htmlToYNodes, appendNodes, createAppendHandler } from './appendToDoc.js'

const docFrom = (html) => TiptapTransformer.toYdoc(generateJSON(html, COLLAB_EXTENSIONS), 'default', COLLAB_EXTENSIONS)
// generateHTML on the server serialises with an xmlns on each element; the
// structure is what matters here.
const htmlOf = (ydoc) => generateHTML(TiptapTransformer.fromYdoc(ydoc, 'default'), COLLAB_EXTENSIONS)
    .replace(/ xmlns="[^"]*"/g, '')

test('the secret must be set, and must match exactly', () => {
    assert.equal(internalSecretOK('s3cret', 's3cret'), true)
    assert.equal(internalSecretOK('s3cret', 's3cre'), false)
    assert.equal(internalSecretOK(undefined, 's3cret'), false)
    assert.equal(internalSecretOK('', ''), false, 'an unset secret must never match, even an empty header')
    assert.equal(internalSecretOK('super-secret-key', undefined), false)
})

test('appended blocks land after what was there, formatting intact', () => {
    const ydoc = docFrom('<h1>Launch sync notes</h1><p>Decisions below.</p>')
    appendNodes(ydoc, htmlToYNodes('<h2>Rollback steps</h2><ol><li><p>Revert the deploy</p></li><li><p>Restore the snapshot</p></li></ol>', COLLAB_EXTENSIONS))
    const html = htmlOf(ydoc)
    assert.ok(html.indexOf('Decisions below.') < html.indexOf('Rollback steps'), html)
    assert.match(html, /<ol><li><p>Revert the deploy<\/p><\/li><li><p>Restore the snapshot<\/p><\/li><\/ol>/)
    assert.match(html, /<h1>Launch sync notes<\/h1>/, 'the existing content is untouched')
})

function fakeRequest({ method = 'POST', url, headers = {}, body = '' }) {
    const r = Readable.from([Buffer.from(body)])
    return Object.assign(r, { method, url, headers })
}
function fakeResponse() {
    return { status: 0, body: '', headersSent: false,
        writeHead(s) { this.status = s; this.headersSent = true },
        end(b) { this.body = b } }
}
async function call(handler, req, instance) {
    const res = fakeResponse()
    let stopped = false
    try { await handler({ request: req, response: res, instance }) } catch (e) { if (e === null) stopped = true; else throw e }
    return { res, stopped }
}

const ID = '11111111-2222-3333-4444-555555555555'

test('the handler ignores other paths, and refuses without a proper secret', async () => {
    const h = createAppendHandler({ getSecret: () => 'k', extensions: COLLAB_EXTENSIONS })
    const other = await call(h, fakeRequest({ url: '/' }), {})
    assert.equal(other.stopped, false, 'other requests fall through to Hocuspocus')

    const unset = await call(createAppendHandler({ getSecret: () => '', extensions: COLLAB_EXTENSIONS }),
        fakeRequest({ url: `/internal/docs/${ID}/append`, headers: { 'x-internal-secret': '' }, body: '{"html":"<p>x</p>"}' }), {})
    assert.equal(unset.res.status, 503)

    const wrong = await call(h, fakeRequest({ url: `/internal/docs/${ID}/append`, headers: { 'x-internal-secret': 'nope' }, body: '{"html":"<p>x</p>"}' }), {})
    assert.equal(wrong.res.status, 401)

    const empty = await call(h, fakeRequest({ url: `/internal/docs/${ID}/append`, headers: { 'x-internal-secret': 'k' }, body: '{"html":"  "}' }), {})
    assert.equal(empty.res.status, 400)
})

test('an authorised append edits the live document and is attributed', async () => {
    const ydoc = docFrom('<p>Existing</p>')
    const opened = []
    const contributors = []
    const instance = {
        async openDirectConnection(name, context) {
            opened.push([name, context.user.id])
            return { transact: async (fn) => fn(ydoc), disconnect: async () => {} }
        },
    }
    const h = createAppendHandler({ getSecret: () => 'k', extensions: COLLAB_EXTENSIONS,
        recordContributor: async (doc, user) => contributors.push([doc, user]) })
    const { res, stopped } = await call(h, fakeRequest({ url: `/internal/docs/${ID}/append`, headers: { 'x-internal-secret': 'k' },
        body: JSON.stringify({ html: '<p>Added by the agent</p>', editor_uuid: 'bot-1' }) }), instance)
    assert.equal(stopped, true)
    assert.equal(res.status, 200, res.body)
    assert.deepEqual(opened, [[ID, 'bot-1']])
    assert.deepEqual(contributors, [[ID, 'bot-1']])
    assert.match(htmlOf(ydoc), /<p>Existing<\/p><p>Added by the agent<\/p>/)
})

test('against a real Hocuspocus: the append reaches the stored document', async () => {
    const { Hocuspocus } = await import('@hocuspocus/server')
    const stored = []
    const hp = new Hocuspocus({
        quiet: true,
        async onLoadDocument() { return docFrom('<p>Before</p>') },
        async onStoreDocument(data) { stored.push(htmlOf(data.document)) },
    })
    const h = createAppendHandler({ getSecret: () => 'k', extensions: COLLAB_EXTENSIONS })
    const { res } = await call(h, fakeRequest({ url: `/internal/docs/${ID}/append`, headers: { 'x-internal-secret': 'k' },
        body: JSON.stringify({ html: '<p>After</p>', editor_uuid: 'bot-1' }) }), hp)
    assert.equal(res.status, 200, res.body)
    assert.ok(stored.length > 0, 'the change must go through onStoreDocument')
    assert.match(stored[stored.length - 1], /<p>Before<\/p><p>After<\/p>/)
})
