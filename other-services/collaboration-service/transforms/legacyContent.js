// Old content moved over as a doc loads (docs programme B2, I5).
//
// Two things changed shape, and a doc's saved Yjs state still holds the old:
//   - a toggle kept its title in a `title` attribute (an input, last writer
//     wins) and a shared `open`; now its first line is its summary, and open
//     or closed is each reader's own (the web's toggleState.ts);
//   - a text colour was the CSS variable itself, var(--camp-sky-ink), or
//     whatever a paste brought; now it is a name, "sky" (N15).
// A doc rebuilt from its HTML body reads the old shapes through the schema
// (schema/collapsible.js, schema/textStyle.js). A doc opened from its Yjs
// state is moved over here, as it loads and before anyone syncs: the title
// becomes the toggle's first line, `open` goes, and a colour becomes its name
// (one that isn't ours goes, as it would vanish in the dark theme). Nothing a
// person wrote is lost.
//
// It is an edit like any other (sent to every tab, buffered, stored), made
// with a Yjs client of its own drawn from the doc and its lineage: two
// servers moving the same doc over at once write the same items, which merge
// as one, so no title is ever doubled. A second run finds nothing to do.

import { createHash } from 'node:crypto'
import * as Y from 'yjs'
import { FIELD, epochIn } from '../lineage.js'
import { colourName } from '../schema/colours.js'

// clientFor is the Yjs client the move is made as, for this doc and lineage.
export function clientFor(docName, lineage) {
    const b = createHash('sha256').update(`legacy-content\n${docName}\n${lineage}`, 'utf8').digest()
    return (b.readUInt32BE(0) >>> 1) || 1
}

const isOldToggle = (el) => el.nodeName === 'collapsible' && (el.getAttribute('title') !== undefined || el.getAttribute('open') !== undefined)

// The colour fixes a text needs, as [index, length, the textStyle it gets].
function colourFixes(text) {
    const fixes = []
    let at = 0
    for (const op of text.toDelta()) {
        const length = typeof op.insert === 'string' ? op.insert.length : 1
        const style = op.attributes?.textStyle
        if (style && style.color != null && colourName(style.color) !== style.color) {
            const name = colourName(style.color)
            const rest = Object.entries(style).filter(([k, v]) => k !== 'color' && v != null)
            fixes.push([at, length, name ? { ...style, color: name } : rest.length ? { ...style, color: null } : null])
        }
        at += length
    }
    return fixes
}

// needsMoving says whether a doc holds anything old, without changing it.
export function needsMoving(fragment) {
    let found = false
    const walk = (parent) => {
        for (const child of parent.toArray()) {
            if (found) return
            if (child instanceof Y.XmlText) found = colourFixes(child).length > 0
            else if (child instanceof Y.XmlElement) {
                if (isOldToggle(child)) found = true
                else walk(child)
            }
        }
    }
    walk(fragment)
    return found
}

// moveOver changes what is old in a fragment, in the caller's transaction,
// and says how many things it changed.
export function moveOver(fragment) {
    let changed = 0
    const walk = (parent) => {
        for (const child of parent.toArray()) {
            if (child instanceof Y.XmlText) {
                for (const [index, length, textStyle] of colourFixes(child)) {
                    child.format(index, length, { textStyle })
                    changed++
                }
            } else if (child instanceof Y.XmlElement) {
                if (isOldToggle(child)) {
                    // Its title as its first line; with none, an empty line,
                    // so its blocks stay its blocks.
                    const title = child.getAttribute('title')
                    const line = new Y.XmlElement('paragraph')
                    if (typeof title === 'string' && title.length) line.insert(0, [new Y.XmlText(title)])
                    child.insert(0, [line])
                    if (title !== undefined) child.removeAttribute('title')
                    if (child.getAttribute('open') !== undefined) child.removeAttribute('open')
                    changed++
                }
                walk(child)
            }
        }
    }
    walk(fragment)
    return changed
}

// moveLegacyContent moves a doc's old content over, as one edit with
// `origin`, and says how many things it changed (0: nothing was old).
export function moveLegacyContent(ydoc, { docName, origin = null }) {
    if (!needsMoving(ydoc.getXmlFragment(FIELD))) return 0
    const copy = new Y.Doc()
    copy.clientID = clientFor(docName, epochIn(ydoc) || '')
    Y.applyUpdate(copy, Y.encodeStateAsUpdate(ydoc))
    let changed = 0
    copy.transact(() => {
        changed = moveOver(copy.getXmlFragment(FIELD))
    })
    if (changed) Y.applyUpdate(ydoc, Y.encodeStateAsUpdate(copy, Y.encodeStateVector(ydoc)), origin)
    copy.destroy()
    return changed
}

// createLegacyContentTransform is what the server runs as a doc loads (its
// afterLoadDocument hook), before the block ids: the edit's user is
// "unknown", so it is never counted as an editor.
export function createLegacyContentTransform({ log = console } = {}) {
    const origin = { context: { user: { id: 'unknown', name: 'Old content' } }, socketId: 'server' }
    return {
        origin,
        give(docName, ydoc) {
            const changed = moveLegacyContent(ydoc, { docName, origin })
            if (changed > 0) log.log(`[Collab] ${docName}: moved ${changed} old toggle${changed === 1 ? '' : 's'} and colours over`)
            return changed
        },
    }
}
