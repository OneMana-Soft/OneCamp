// Keeping the Hocuspocus Redis extension's failures inside it.
//
// The extension takes a Redis lock before every save and publishes every change
// and awareness update. Hocuspocus calls some of those hooks without waiting
// (onChange, onAwarenessUpdate, beforeBroadcastStateless), so a Redis error
// there was an unhandled rejection; and an unexpected lock error ("Redis is
// down") was rethrown out of the save, the same unhandled rejection that took
// the whole service down (saveFailures.js).
//
// guardRedisExtension wraps those hooks:
//   - the lock: another instance holding it is still a quiet skip (the
//     extension throws an Error with no message, and Hocuspocus skips the save
//     without a word); any other lock error is logged and the save goes ahead
//     without the lock, which on one instance changes nothing and on several
//     at worst saves the same document twice;
//   - publishing and subscribing: logged (at most once a minute per hook) and
//     dropped; this instance carries on, and other instances catch up from the
//     next update.

const PUBLISHING_HOOKS = ['onChange', 'onAwarenessUpdate', 'beforeBroadcastStateless', 'afterLoadDocument', 'afterUnloadDocument']

export function guardRedisExtension(extension, { log = console, now = Date.now } = {}) {
    const lastLogged = new Map()
    const logSometimes = (hook, message) => {
        const t = now()
        if (t - (lastLogged.get(hook) || 0) < 60_000) return
        lastLogged.set(hook, t)
        log.error(message)
    }

    const store = extension.onStoreDocument?.bind(extension)
    if (store) {
        extension.onStoreDocument = async (payload) => {
            try {
                return await store(payload)
            } catch (e) {
                if (!e?.message) throw e
                log.error(`[Collab] Redis lock for ${payload?.documentName} failed (${e.message}); saving without it`)
            }
        }
    }

    for (const hook of PUBLISHING_HOOKS) {
        const original = extension[hook]?.bind(extension)
        if (!original) continue
        extension[hook] = async (payload) => {
            try {
                return await original(payload)
            } catch (e) {
                logSometimes(hook, `[Collab] Redis ${hook} for ${payload?.documentName} failed: ${e?.message || e}`)
            }
        }
    }
    return extension
}
