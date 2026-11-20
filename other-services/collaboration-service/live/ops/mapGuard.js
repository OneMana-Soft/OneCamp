// Maps on a board's Y doc that a socket may not write (or that some sockets
// may not): any such socket's change is put back as it was, in a transaction
// of this service's own (origin), so every tab takes the put-back. A change is
// repaired, never dropped: dropping a Yjs update would leave that tab's later
// updates waiting for ever. Changes with no socket behind them (loading, this
// service's ops, pruning) stand.
//
// guestStrokes keeps its record of who drew what from every socket,
// facilitationGuard keeps a board's facilitation from its call guests (V-462),
// protect keeps its record of protected objects from every socket (miro-44),
// and authorGuard keeps every socket writing as itself (V-965).

// newer is v as a version every tab takes over the ones given: a board's
// elements are merged by version, so a put-back element goes in as newer than
// both what it was and what it was changed to (protect, elementTypes).
export function newer(v, ...against) {
    const version = Math.max(v.version || 0, ...against.map((a) => a?.version || 0)) + 1
    return { ...v, version, versionNonce: Math.floor(Math.random() * 2 ** 31), updated: Date.now() }
}

// putBackSocketWrites keeps map against the sockets' changes that
// refuses(context, key, change, map) refuses, key by key (every socket's every
// change, unless told otherwise; map.get(key) is the change as written). A
// refused change is put back as it was, and heard(context, key, why) hears of
// it, why being refuses' answer. refuses may instead answer { stamp: value }:
// the change stands as value (authorGuard.js stamps a writer's own name), or
// { stamp: value, told: why }: value puts it back (a newer version of an
// element), and heard hears why.
export function putBackSocketWrites(doc, map, origin, refuses = () => true, heard = () => {}) {
    map.observe((event) => {
        const context = event.transaction.origin?.context
        if (!context) return
        const repairs = []
        event.changes.keys.forEach((change, key) => {
            const why = refuses(context, key, change, map)
            if (why) repairs.push({ key, change, why })
        })
        if (repairs.length === 0) return
        doc.transact(() => {
            for (const { key, change, why } of repairs) {
                if (why.stamp !== undefined) map.set(key, why.stamp)
                else if (change.action === 'add') map.delete(key)
                else map.set(key, change.oldValue)
            }
        }, origin)
        for (const { key, why } of repairs) {
            const told = why.stamp === undefined ? why : why.told
            if (told) heard(context, key, told)
        }
    })
}
