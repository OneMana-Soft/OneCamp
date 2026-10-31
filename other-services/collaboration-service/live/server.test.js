import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createLiveServer } from './server.js'
import { BOARD_ROUTES, LIVE_ROUTES, PEOPLE_ROUTES } from './routes.js'

const SECRET = 'a-test-secret-long-enough-to-count-0123456789'
const DOC = '0b6f2f4e-9c1a-4f53-8a57-2c1f0d6e7a11'

async function withServer(routes, run, peopleRoutes) {
    const server = createLiveServer({ getSecret: () => SECRET, routes, peopleRoutes })
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
    const base = `http://127.0.0.1:${server.address().port}`
    try {
        await run(base)
    } finally {
        await new Promise((resolve) => server.close(resolve))
    }
}

function call(base, path, { secret = SECRET, method = 'POST', body = '{}' } = {}) {
    const headers = { 'Content-Type': 'application/json' }
    if (secret) headers['X-Internal-Secret'] = secret
    return fetch(base + path, { method, headers, body: method === 'GET' ? undefined : body })
}

test('the Live Doc Writer answers its ops, and nothing else', async () => {
    assert.deepEqual([...LIVE_ROUTES.keys()].sort(), ['append', 'applyProposal', 'insertAfter', 'placeSubpage', 'read', 'reauth', 'removeSubpage', 'render', 'replaceAll', 'replaceBlocks', 'seed', 'setTaskChecks'])
    // A board takes no content op (its canvas is no doc): only its reauth and
    // a call's guests' drawing (calls B).
    assert.deepEqual([...BOARD_ROUTES.keys()], ['reauth', 'guestsSnapshot', 'guestsBegin', 'undoGuests'])
    assert.deepEqual([...PEOPLE_ROUTES.keys()], ['close'])
    await withServer(LIVE_ROUTES, async (base) => {
        const res = await call(base, `/live/docs/${DOC}/rewriteHistory`)
        assert.equal(res.status, 404)
    })
})

test('every registered op is a function the server can call', () => {
    for (const [name, op] of [...LIVE_ROUTES, ...BOARD_ROUTES, ...PEOPLE_ROUTES]) assert.equal(typeof op, 'function', name)
    assert.ok(LIVE_ROUTES.has('applyProposal')) // A9
})

test('a caller without the internal secret is refused before anything else', async () => {
    const routes = new Map([['echo', async () => 'reached']])
    await withServer(routes, async (base) => {
        assert.equal((await call(base, `/live/docs/${DOC}/echo`, { secret: '' })).status, 401)
        assert.equal((await call(base, `/live/docs/${DOC}/echo`, { secret: 'wrong' })).status, 401)
    })
    // No secret configured: nobody gets in.
    const server = createLiveServer({ getSecret: () => '', routes })
    await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve))
    try {
        const res = await call(`http://127.0.0.1:${server.address().port}`, `/live/docs/${DOC}/echo`, { secret: 'anything' })
        assert.equal(res.status, 401)
    } finally {
        await new Promise((resolve) => server.close(resolve))
    }
})

test('an op gets its doc and body, and its answer or refusal is passed back', async () => {
    const routes = new Map([
        ['echo', async ({ docName, body }) => ({ docName, got: body.html })],
        ['refuse', async () => { throw Object.assign(new Error('no such block'), { status: 404 }) }],
        ['crash', async () => { throw new Error('boom') }],
    ])
    await withServer(routes, async (base) => {
        const ok = await call(base, `/live/docs/${DOC}/echo`, { body: JSON.stringify({ html: '<p>x</p>' }) })
        assert.equal(ok.status, 200)
        assert.deepEqual((await ok.json()).data, { docName: DOC, got: '<p>x</p>' })

        const refused = await call(base, `/live/docs/${DOC}/refuse`)
        assert.equal(refused.status, 404)
        assert.equal((await refused.json()).msg, 'no such block')

        const crashed = await call(base, `/live/docs/${DOC}/crash`)
        assert.equal(crashed.status, 500)
        assert.equal((await crashed.json()).msg, 'the op failed') // no internals leak out

        assert.equal((await call(base, `/live/docs/${DOC}/echo`, { method: 'GET' })).status, 405)
        assert.equal((await call(base, `/live/docs/${DOC}/echo`, { body: 'not json' })).status, 400)
        assert.equal((await call(base, `/live/docs/not-a-uuid/echo`)).status, 404)
    })
})

test('a body over the cap is refused, and the refusal arrives', async () => {
    const routes = new Map([['echo', async () => 'reached']])
    await withServer(routes, async (base) => {
        const res = await call(base, `/live/docs/${DOC}/echo`, { body: JSON.stringify({ html: 'x'.repeat(17 * 1024 * 1024) }) })
        assert.equal(res.status, 413)
    })
})

test('ops about people are answered on a path of their own, behind the same secret', async () => {
    const peopleRoutes = new Map([['echo', async ({ body }) => ({ got: body.user_ids })]])
    await withServer(new Map(), async (base) => {
        assert.equal((await call(base, '/live/people/echo', { secret: '' })).status, 401)
        const ok = await call(base, '/live/people/echo', { body: JSON.stringify({ user_ids: ['sam'] }) })
        assert.equal(ok.status, 200)
        assert.deepEqual((await ok.json()).data, { got: ['sam'] })
        assert.equal((await call(base, '/live/people/rewriteHistory')).status, 404)
        assert.equal((await call(base, `/live/people/${DOC}/echo`)).status, 404)
        assert.equal((await call(base, `/live/docs/${DOC}/echo`)).status, 404, "a doc's ops are not people's")
    }, peopleRoutes)
})
