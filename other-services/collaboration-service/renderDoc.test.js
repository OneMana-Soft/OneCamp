import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Readable } from 'node:stream'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { generateHTML } from '@tiptap/html'
import { COLLAB_EXTENSIONS } from './extensions.js'
import { EVERY_BLOCK } from './everyBlock.fixture.js'
import { encodeState } from './docState.js'
import { createRenderHandler, stateToHTML } from './renderDoc.js'

// The repair command's view of a saved state: the body it would have been.
const ID = '7d0e3c3e-5b8a-4c9e-9a51-3f1f0b3b9a11'
const state = encodeState(TiptapTransformer.toYdoc(EVERY_BLOCK, 'default', COLLAB_EXTENSIONS))

test('a saved state is written as the body the save path writes', () => {
    assert.equal(stateToHTML(state, COLLAB_EXTENSIONS), generateHTML(EVERY_BLOCK, COLLAB_EXTENSIONS))
})

const call = async ({ secret = 's3cret', body = { state }, open = false, path = `/internal/docs/${ID}/render` } = {}) => {
    const request = Readable.from([Buffer.from(JSON.stringify(body))])
    Object.assign(request, { url: path, method: 'POST', headers: { 'x-internal-secret': secret } })
    const response = {
        status: 0, body: '', headersSent: false,
        writeHead(s) { this.status = s; this.headersSent = true },
        end(b) { this.body = b },
    }
    const handler = createRenderHandler({ getSecret: () => 's3cret', extensions: COLLAB_EXTENSIONS })
    let stopped = false
    try {
        await handler({ request, response, instance: { documents: new Map(open ? [[ID, {}]] : []) } })
    } catch (e) {
        stopped = e === null
    }
    return { stopped, status: response.status, json: response.body ? JSON.parse(response.body) : null }
}

test('the render route answers with the body and whether the doc is open', async () => {
    const r = await call()
    assert.equal(r.status, 200)
    assert.ok(r.stopped)
    assert.match(r.json.data.html, /data-type="table-embed"/)
    assert.equal(r.json.data.open, false)
    assert.equal((await call({ open: true, body: {} })).json.data.open, true)
})

test('the render route trusts only the internal secret', async () => {
    assert.equal((await call({ secret: 'wrong' })).status, 401)
})

test('other paths pass through to the next handler', async () => {
    const r = await call({ path: `/internal/docs/${ID}/append` })
    assert.equal(r.stopped, false)
    assert.equal(r.status, 0)
})
