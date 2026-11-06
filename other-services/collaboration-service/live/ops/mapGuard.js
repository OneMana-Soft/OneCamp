// Maps on a board's Y doc that a socket may not write (or that some sockets
// may not): any such socket's change is put back as it was, in a transaction
// of this service's own (origin), so every tab takes the put-back. A change is
// repaired, never dropped: dropping a Yjs update would leave that tab's later
// updates waiting for ever. Changes with no socket behind them (loading, this
// service's ops, pruning) stand.
//
// guestStrokes keeps its record of who drew what from every socket,
// facilitationGuard keeps a board's facilitation from its call guests (V-462),
// and protect keeps its record of protected objects from every socket (miro-44).

// putBackSocketWrites keeps map against the sockets refuses(context) names
// (every socket, unless told otherwise).
export function putBackSocketWrites(doc, map, origin, refuses = () => true) {
    map.observe((event) => {
        const context = event.transaction.origin?.context
        if (!context || !refuses(context)) return
        doc.transact(() => {
            event.changes.keys.forEach((change, key) => {
                if (change.action === 'add') map.delete(key)
                else map.set(key, change.oldValue)
            })
        }, origin)
    })
}
