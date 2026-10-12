// Which editors may change a doc: a tab whose editor is older than this server.
//
// WHY. y-prosemirror deletes, from the browser's own copy of the doc, every
// element its editor's schema cannot hold, and the browser then sends that
// deletion like any edit. A tab opened before a deploy that added a block type
// therefore deletes that block for everyone the first time it meets one.
//
// So the web app sends the hash of the schema manifest it was built with (the
// `schema` URL parameter), and a doc connection from a manifest this server has
// moved on from is opened read-only: its edits are dropped here, and the tab is
// told to reload ("schema-outdated", a stateless message). A tab that sends no
// hash (built before this check) or one this server does not know (built after
// it) edits as before: refusing them would lock every doc whenever the web app
// and this service are deployed apart.
//
// schema-history.json lists every manifest hash this server has shipped, oldest
// first; the last is the current manifest's (schemaParity.test.js holds that).

import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'

// manifestHash is the hash both sides compute: SHA-256, hex, of the manifest
// re-serialised compactly (JSON.stringify of the parsed file), so whitespace
// does not count. The web app pins the same value (schemaManifest.ts).
export const manifestHash = (manifestText) =>
    createHash('sha256').update(JSON.stringify(JSON.parse(manifestText)), 'utf8').digest('hex')

const here = new URL('.', import.meta.url)
export const SCHEMA_HASH = manifestHash(readFileSync(new URL('schema-manifest.json', here), 'utf8'))
export const SCHEMA_HISTORY = JSON.parse(readFileSync(new URL('schema-history.json', here), 'utf8')).hashes

// schemaVerdict is 'current', 'outdated' (a manifest this server has shipped
// before), 'missing' (no hash sent) or 'unknown'.
export function schemaVerdict(hash, { current = SCHEMA_HASH, history = SCHEMA_HISTORY } = {}) {
    if (typeof hash !== 'string' || hash === '') return 'missing'
    if (hash === current) return 'current'
    return history.includes(hash) ? 'outdated' : 'unknown'
}

// The stateless message an outdated tab gets. The web app shows "A newer
// version of OneCamp is ready. Reload to keep editing" and stops editing.
export const SCHEMA_OUTDATED_MESSAGE = JSON.stringify({ type: 'schema-outdated' })
