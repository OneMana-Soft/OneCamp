// The api client's retries (index.js): an internal API call is sent again,
// three times, after 2, 4 and 8 s, when the API may answer differently next
// time: no answer, a 5xx, 408 or 429. A 4xx is an answer (not found, refused,
// not allowed), so it is returned at once, and so is a call its caller
// aborted (the guest re-check giving up at its bound, live/ops/reauth.js
// askWithin): nobody waits for its answer.

import axios from 'axios'

const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms))

// retryUnanswered is the client's onRejected: api sends a request config
// again, wait(ms) waits before each try.
export function retryUnanswered(api, wait = pause) {
    return async (error) => {
        const { config } = error
        const status = error?.response?.status
        const final = typeof status === 'number' && status >= 400 && status < 500 && status !== 408 && status !== 429
        const aborted = axios.isCancel(error) || config?.signal?.aborted === true
        if (!config || final || aborted || config.__retryCount >= 3) {
            return Promise.reject(error)
        }
        config.__retryCount = config.__retryCount || 0
        config.__retryCount += 1
        await wait(Math.pow(2, config.__retryCount) * 1000)
        return api(config)
    }
}
