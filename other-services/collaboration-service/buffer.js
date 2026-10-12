// Everything this service keeps in Redis so an edit survives until it is saved.
//
//   collab:updates:<doc>     every incremental update since the last good save,
//                            replayed on load (crash recovery);
//   collab:unsaved:<doc>     the doc's whole state, written when a save fails, so
//                            a restart reopens exactly what was in memory, edits
//                            included (the updates alone cannot attach to a doc
//                            rebuilt from HTML: they belong to the old copy);
//   collab:quarantine:<doc>  bytes that could not be replayed or saved, kept for
//                            recovery and never replayed again;
//   collab:quarantine-state:<doc>
//                            the doc's whole state when its unsaved edits were
//                            quarantined, kept apart from the list above: the
//                            list keeps its last 200 entries, and a minute of
//                            typing after the state used to push it out;
//   collab:flag:<doc>        why the doc was quarantined, for an operator.
//
// Every call is best-effort: a Redis outage must never stop editing or saving,
// so failures are logged and the caller carries on.

import * as Y from 'yjs'

export const UPDATES_TTL_SECONDS = 60 * 60
// While a doc's saves keep failing, its edits are kept far longer than the
// usual hour: an outage of the API or the graph must not expire them.
export const HOLD_TTL_SECONDS = 7 * 24 * 60 * 60
export const QUARANTINE_TTL_SECONDS = 30 * 24 * 60 * 60
const QUARANTINE_MAX_ENTRIES = 200

export const keys = {
    updates: (name) => `collab:updates:${name}`,
    unsaved: (name) => `collab:unsaved:${name}`,
    quarantine: (name) => `collab:quarantine:${name}`,
    quarantineState: (name) => `collab:quarantine-state:${name}`,
    flag: (name) => `collab:flag:${name}`,
}

const b64 = (u8) => Buffer.from(u8).toString('base64')

export function createUpdateBuffer(redis, { log = console } = {}) {
    const quiet = async (what, fn, fallback) => {
        try {
            return await fn()
        } catch (e) {
            log.error(`[Collab] Redis ${what} failed:`, e?.message || e)
            return fallback
        }
    }

    const buffer = {
        // push appends one incremental update.
        async push(name, update) {
            if (!update || !update.length) return
            // The hour restarts with each update. While saves fail, every retry
            // extends it again (hold) and refreshes the whole state (keepUnsaved).
            await quiet(`buffer ${name}`, async () => {
                await redis.rpush(keys.updates(name), b64(update))
                await redis.expire(keys.updates(name), UPDATES_TTL_SECONDS)
            })
        },

        async count(name) {
            return quiet(`count ${name}`, () => redis.llen(keys.updates(name)), 0)
        },

        // trim drops the first n updates, the ones a save just made durable,
        // keeping any that arrived during it.
        async trim(name, n) {
            if (!n || n <= 0) return
            await quiet(`trim ${name}`, () => redis.ltrim(keys.updates(name), n, -1))
        },

        // replay applies the buffered updates to a doc. An update that cannot be
        // applied, or that `refuse(update, ydoc)` turns away (a second copy of
        // the doc: lineage.js), is moved to quarantine, so it is never replayed
        // again, and the doc is flagged. Returns how many were applied.
        async replay(name, ydoc, { refuse } = {}) {
            const items = await quiet(`read ${name}`, () => redis.lrange(keys.updates(name), 0, -1), [])
            if (!Array.isArray(items) || items.length === 0) return 0
            let applied = 0
            const poison = []
            for (const item of items) {
                try {
                    const update = Buffer.from(item, 'base64')
                    if (refuse && refuse(update, ydoc)) {
                        poison.push({ item, error: 'it would add a second copy of the doc' })
                        continue
                    }
                    Y.applyUpdate(ydoc, update)
                    applied++
                } catch (e) {
                    poison.push({ item, error: e?.message || String(e) })
                }
            }
            if (poison.length) {
                for (const p of poison) {
                    await buffer.quarantine(name, 'replay', p.error, p.item)
                    await quiet(`drop ${name}`, () => redis.lrem(keys.updates(name), 1, p.item))
                }
                await buffer.flag(name, 'replay', `${poison.length} buffered update(s) could not be applied: ${poison[0].error}`)
            }
            log.log(`[Collab] Replayed ${applied} buffered update(s) for ${name}${poison.length ? `, quarantined ${poison.length}` : ''}`)
            return applied
        },

        // hold keeps the buffered updates while saves are failing.
        async hold(name) {
            await quiet(`hold ${name}`, () => redis.expire(keys.updates(name), HOLD_TTL_SECONDS))
        },

        // keepUnsaved stores the doc's whole state after a failed save.
        async keepUnsaved(name, ydoc) {
            let state
            try {
                state = b64(Y.encodeStateAsUpdate(ydoc))
            } catch (e) {
                log.error(`[Collab] Could not encode the unsaved state of ${name}:`, e?.message || e)
                return false
            }
            return quiet(`keep unsaved ${name}`, async () => {
                await redis.set(keys.unsaved(name), state, 'EX', HOLD_TTL_SECONDS)
                return true
            }, false)
        },

        // unsaved is the state kept by keepUnsaved as a Y.Doc, or null. A state
        // that does not decode is quarantined instead.
        async unsaved(name) {
            const state = await quiet(`read unsaved ${name}`, () => redis.get(keys.unsaved(name)), null)
            if (!state) return null
            try {
                const ydoc = new Y.Doc()
                Y.applyUpdate(ydoc, Buffer.from(state, 'base64'))
                return ydoc
            } catch (e) {
                await buffer.quarantine(name, 'unsaved', e?.message || String(e), state)
                await buffer.flag(name, 'unsaved', `the kept unsaved state could not be applied: ${e?.message || e}`)
                await buffer.clearUnsaved(name)
                return null
            }
        },

        async clearUnsaved(name) {
            await quiet(`clear unsaved ${name}`, () => redis.del(keys.unsaved(name)))
        },

        // quarantine keeps bytes for recovery, out of every replay path.
        async quarantine(name, source, reason, bytesB64) {
            const entry = JSON.stringify({ at: new Date().toISOString(), source, reason, bytes: bytesB64 })
            await quiet(`quarantine ${name}`, async () => {
                await redis.rpush(keys.quarantine(name), entry)
                await redis.ltrim(keys.quarantine(name), -QUARANTINE_MAX_ENTRIES, -1)
                await redis.expire(keys.quarantine(name), QUARANTINE_TTL_SECONDS)
            })
            log.error(`[Collab] QUARANTINED ${source} bytes of ${name} (${reason}); kept in ${keys.quarantine(name)}`)
        },

        // quarantineAll moves a doc's kept edits (its unsaved state and every
        // buffered update) to quarantine, so nothing replays them. The whole
        // state goes under its own key (the latest one is kept), so the
        // updates quarantined after it can never trim it away.
        async quarantineAll(name, reason, ydoc) {
            // A retry that could not even open the doc has no copy in memory:
            // the whole state its failed save kept is what is quarantined, not
            // dropped with the rest below.
            if (!ydoc) ydoc = await buffer.unsaved(name)
            if (ydoc) {
                let state = null
                try {
                    state = b64(Y.encodeStateAsUpdate(ydoc))
                } catch (e) {
                    log.error(`[Collab] Could not encode ${name} for quarantine:`, e?.message || e)
                }
                if (state) {
                    const kept = JSON.stringify({ at: new Date().toISOString(), source: 'state', reason, bytes: state })
                    const ok = await quiet(`quarantine state ${name}`, async () => {
                        await redis.set(keys.quarantineState(name), kept, 'EX', QUARANTINE_TTL_SECONDS)
                        return true
                    }, false)
                    if (ok) log.error(`[Collab] QUARANTINED the whole state of ${name} (${reason}); kept in ${keys.quarantineState(name)}`)
                }
            }
            const items = await quiet(`read ${name}`, () => redis.lrange(keys.updates(name), 0, -1), [])
            for (const item of items || []) await buffer.quarantine(name, 'update', reason, item)
            await quiet(`drop ${name}`, () => redis.del(keys.updates(name)))
            await buffer.clearUnsaved(name)
        },

        async flag(name, source, reason) {
            await quiet(`flag ${name}`, () =>
                redis.set(keys.flag(name), JSON.stringify({ at: new Date().toISOString(), source, reason }), 'EX', QUARANTINE_TTL_SECONDS))
        },

        // unsavedNames lists the docs whose last save failed, for the retry
        // sweep at startup.
        async unsavedNames() {
            return quiet('scan unsaved', async () => {
                const prefix = keys.unsaved('')
                const names = []
                let cursor = '0'
                do {
                    const [next, batch] = await redis.scan(cursor, 'MATCH', `${prefix}*`, 'COUNT', 200)
                    cursor = String(next)
                    for (const k of batch) names.push(k.slice(prefix.length))
                } while (cursor !== '0')
                return names
            }, [])
        },
    }
    return buffer
}
