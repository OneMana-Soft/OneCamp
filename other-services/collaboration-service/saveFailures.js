// What happens when a doc or board cannot be saved.
//
// WHY THIS EXISTS. A save that threw left the Hocuspocus debouncer with an
// unhandled rejection, and index.js exits on those: the whole service went down
// about five seconds after the edit, every doc and board dropped, and on
// restart the Redis buffer replayed the same edit and it went down again, for
// as long as anyone edited that doc (11 Oct 2026). A failed save of the API or
// the graph, or a Redis lock error, did the same.
//
// Now a failed save never leaves the save hook. Instead:
//   - it is logged with the doc's name and the attempt;
//   - the edits stay: the buffered updates are held, the doc's whole state is
//     kept in Redis (a restart reopens it), and the doc is pinned in memory so
//     it does not unload while unsaved;
//   - the save is tried again on a timer, 5 s, 10 s, 20 s ... up to 5 min apart,
//     whether or not anyone is still editing;
//   - a failure that is not going to pass (the API refusing the content with a
//     4xx, or content that cannot be converted) quarantines the doc's unsaved
//     edits after a few tries: their bytes are kept for recovery, never replayed
//     again, the doc is flagged, and the service carries on;
//   - a doc that is gone (the API answers 404 or 410: it was deleted, or the
//     retention run archived it, while it was open) is let go at once: its
//     unsaved edits are quarantined, its tabs are told, and nothing saves it
//     again until it is opened anew (restored).
//
// AND IT ENDS. Quarantining lets go of the pinned copy, and letting go of a
// direct connection saves the doc, so a refused save used to start a new
// failure, pin the doc again, and quarantine it again, every few seconds until
// a restart (12 Oct 2026). A doc whose edits were quarantined is not saved
// again until someone edits it, and a gone doc is not saved at all.

export const RETRY_DELAYS_MS = [5_000, 10_000, 20_000, 40_000, 80_000, 160_000, 300_000]
export const POISON_AFTER = 3

// isGone is whether a failed save says the doc is no longer there to save:
// deleted (410) or not found (404). Trying again cannot help.
export function isGone(err) {
    const status = err?.response?.status
    return status === 404 || status === 410
}

// isTransient is whether a failure may pass on its own: the network, a timeout,
// a 5xx, 408, 425 or 429, or Redis. A 4xx answer, or content that could not be
// converted (`deterministic`), will fail the same way every time.
export function isTransient(err) {
    if (err?.deterministic) return false
    const status = err?.response?.status
    if (typeof status === 'number') return status >= 500 || status === 408 || status === 425 || status === 429
    return true
}

const describe = (err) => {
    const status = err?.response?.status
    return `${status ? `HTTP ${status}: ` : ''}${err?.response?.data?.msg || err?.message || String(err)}`
}

export function createSaveFailures({
    buffer,
    // open(name) is a Hocuspocus direct connection to the doc (loading it if
    // it is not in memory); its disconnect() saves it.
    open,
    log = console,
    delays = RETRY_DELAYS_MS,
    poisonAfter = POISON_AFTER,
    timers = { setTimeout, clearTimeout },
    // onGone(name) tells a gone doc's tabs (collabServer.js).
    onGone = () => {},
}) {
    const state = new Map() // name -> { attempts, refused, timer, pin }
    // Docs not to save: gone ones, until they are opened again; and docs whose
    // unsaved edits were just quarantined, until someone edits them again.
    const gone = new Set()
    const settled = new Set()
    let stopping = false
    const entry = (name) => {
        let s = state.get(name)
        if (!s) {
            s = { attempts: 0, refused: 0, timer: null, pin: null }
            state.set(name, s)
        }
        return s
    }

    const unpin = (s) => {
        const pin = s.pin
        s.pin = null
        if (!pin) return
        // Later, not now: this runs inside a save, and a direct connection's
        // disconnect saves too, so waiting for it here would wait on itself.
        timers.setTimeout(() => {
            pin.disconnect().catch((e) => log.error('[Collab] Unpinning failed:', e?.message || e))
        }, 0)
    }

    const schedule = (name, s, delay) => {
        if (s.timer || stopping) return
        s.timer = timers.setTimeout(() => {
            s.timer = null
            retry(name)
        }, delay)
        if (typeof s.timer?.unref === 'function') s.timer.unref()
    }

    // retry saves the doc again: a direct connection loads it if it is not in
    // memory (from the kept state and buffer), and disconnecting saves it,
    // through the same hooks as any save.
    async function retry(name) {
        try {
            const connection = await open(name)
            await connection.disconnect()
        } catch (err) {
            // Opening failed (the API or graph is down): no save hook ran.
            await failures.failed(name, err, null)
        }
    }

    const failures = {
        // failed records a failed save and decides what happens next. It never
        // throws.
        async failed(name, err, document) {
            try {
                const s = entry(name)
                s.attempts++
                const transient = isTransient(err)
                s.refused = transient ? 0 : s.refused + 1
                log.error(`[Collab] SAVE FAILED for ${name} (attempt ${s.attempts}, ${transient ? 'will retry' : `refused ${s.refused}/${poisonAfter}`}): ${describe(err)}. The stored copy is unchanged; the edits are kept.`)

                if (isGone(err)) {
                    await failures.letGo(name, describe(err), document)
                    return
                }
                if (!transient && s.refused >= poisonAfter) {
                    await failures.quarantine(name, describe(err), document)
                    return
                }
                await buffer.hold(name)
                if (document) {
                    await buffer.keepUnsaved(name, document)
                    if (!s.pin && !stopping) {
                        try {
                            s.pin = await open(name)
                        } catch (e) {
                            log.error(`[Collab] Could not pin ${name} in memory:`, e?.message || e)
                        }
                    }
                }
                schedule(name, s, delays[Math.min(s.attempts - 1, delays.length - 1)])
            } catch (e) {
                log.error(`[Collab] Handling a failed save of ${name} failed:`, e?.message || e)
            }
        },

        // succeeded clears a doc's failure state after a good save.
        async succeeded(name) {
            settled.delete(name)
            const s = state.get(name)
            if (!s) return
            state.delete(name)
            if (s.timer) timers.clearTimeout(s.timer)
            unpin(s)
            await buffer.clearUnsaved(name)
            log.log(`[Collab] ${name} saved again after ${s.attempts} failed attempt(s)`)
        },

        // quarantine gives up on a doc's unsaved edits: their bytes go to
        // quarantine, nothing replays them, the doc is flagged.
        async quarantine(name, reason, document) {
            const s = entry(name)
            if (s.timer) timers.clearTimeout(s.timer)
            state.delete(name)
            // Not saved again until someone edits it: letting go of the pin
            // below saves the doc, which would be refused again and start over.
            settled.add(name)
            unpin(s)
            await buffer.quarantineAll(name, reason, document)
            await buffer.flag(name, 'save', reason)
            log.error(`[Collab] DOC ${name} FLAGGED: its saves were refused ${s.refused} times (${reason}). Its unsaved edits are quarantined; the stored copy is unchanged.`)
        },

        // letGo gives up on a doc that is gone (deleted, or archived by the
        // retention run, while it was open): its unsaved edits go to
        // quarantine once, its tabs are told (onGone), and it is not saved
        // again until it is opened anew. Once per doc.
        async letGo(name, reason, document) {
            if (gone.has(name)) return
            gone.add(name)
            const s = state.get(name)
            state.delete(name)
            if (s) {
                if (s.timer) timers.clearTimeout(s.timer)
                unpin(s)
            }
            await buffer.quarantineAll(name, reason, document)
            await buffer.flag(name, 'gone', reason)
            log.error(`[Collab] DOC ${name} IS GONE (${reason}): it was deleted or archived while open. Its unsaved edits are quarantined, its tabs are told, and it is not saved again.`)
            try {
                onGone(name)
            } catch (e) {
                log.error(`[Collab] Could not tell the tabs of ${name} it is gone:`, e?.message || e)
            }
        },

        // skipSave is whether a save of the doc must not be tried: it is gone,
        // or its edits were quarantined and nobody has edited it since.
        skipSave: (name) => gone.has(name) || settled.has(name),
        isGone: (name) => gone.has(name),
        // edited: someone changed the doc, so a quarantined doc saves again.
        edited: (name) => {
            settled.delete(name)
        },
        // opened: the doc was loaded anew (a gone doc that was restored), so
        // it saves as any doc does.
        opened: (name) => {
            gone.delete(name)
            settled.delete(name)
        },

        // recover retries the docs whose saves failed before a restart.
        async recover(names, gapMs = 1_000) {
            names.forEach((name, i) => schedule(name, entry(name), (i + 1) * gapMs))
            if (names.length) log.log(`[Collab] Retrying ${names.length} doc(s) left unsaved before the restart`)
        },

        pending: (name) => state.has(name),

        // shutdown stops retrying and lets go of every pinned doc, so the
        // server can unload them and stop. A doc still unsaved keeps its whole
        // state in Redis, and the next start retries it (recover).
        async shutdown() {
            stopping = true
            const pins = []
            for (const s of state.values()) {
                if (s.timer) timers.clearTimeout(s.timer)
                s.timer = null
                if (s.pin) pins.push(s.pin)
                s.pin = null
            }
            for (const pin of pins) {
                await pin.disconnect().catch((e) => log.error('[Collab] Unpinning failed:', e?.message || e))
            }
        },
    }
    return failures
}
