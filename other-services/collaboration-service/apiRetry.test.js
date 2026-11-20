// The api client's retries (apiRetry.js): an ask the API may answer
// differently next time is sent again after 2, 4 and 8 s; an ask its caller
// aborted (the guest re-check giving up at its bound, live/ops/reauth.js
// askWithin) is not, since nobody waits for its answer.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import axios from 'axios'
import { retryUnanswered } from './apiRetry.js'

// client wires retryUnanswered to a stub api that fails each try with
// fail(config), back through the same onRejected as axios's interceptor
// does; its waits are recorded and end at once.
function client(fail) {
    const calls = []
    const waits = []
    const api = async (config) => {
        calls.push(config)
        return onRejected(fail(config))
    }
    const onRejected = retryUnanswered(api, async (ms) => {
        waits.push(ms)
    })
    return { calls, waits, onRejected }
}

const unavailable = (config) => Object.assign(new Error('Request failed with status code 503'), { config, response: { status: 503 } })

for (const [what, failure] of [
    ['canceled (an abort while it was out)', (config) => new axios.CanceledError(null, config)],
    ['failed after its signal was aborted', (config) => Object.assign(new Error('socket hang up'), { code: 'ECONNRESET', config })],
]) {
    test(`an ask its caller aborted, ${what}, is rejected at once: no second call, no wait`, async () => {
        const abort = new AbortController()
        abort.abort()
        const { calls, waits, onRejected } = client(failure)
        const error = failure({ url: 'http://api/boardColab/guestAuthorize', signal: abort.signal })
        await assert.rejects(onRejected(error), (e) => e === error)
        assert.equal(calls.length, 0, 'not sent again')
        assert.deepEqual(waits, [], 'no wait before a retry')
    })
}

test('an ask the API answered 503 is still sent again three times, after 2, 4 and 8 s', async () => {
    const { calls, waits, onRejected } = client(unavailable)
    const signal = new AbortController().signal // live: never aborted
    await assert.rejects(onRejected(unavailable({ url: 'http://api/boardColab/guestAuthorize', signal })), /503/)
    assert.equal(calls.length, 3, 'three retries')
    assert.deepEqual(waits, [2000, 4000, 8000])
})

test('an answer (a 404) is returned at once', async () => {
    const { calls, waits, onRejected } = client(unavailable)
    const error = Object.assign(new Error('Request failed with status code 404'), { config: { url: 'http://api/x' }, response: { status: 404 } })
    await assert.rejects(onRejected(error), (e) => e === error)
    assert.equal(calls.length, 0)
    assert.deepEqual(waits, [])
})
