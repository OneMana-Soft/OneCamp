// How every Live Doc Writer op that changes a doc runs (live/routes.js).
//
// 1. The doc is opened through a Hocuspocus direct connection: loaded through
//    the same onLoadDocument hook as any editor (a doc that cannot be read is
//    refused, never written into empty), or joined where it is already open.
// 2. The change is made in ONE Yjs transaction on that copy, so everyone with
//    the doc open sees it arrive at once, and nothing they type is overwritten
//    (invariants I1 and I2). Everything that could refuse the op (no such
//    block, a doc that already has content) is checked inside that same
//    synchronous step, so the doc cannot move between the check and the write.
// 3. It is attributed from what the API sends, never from a client (I11): the
//    person, or the agent and the person who asked it, goes into the doc's
//    contributors for the save that stores the change (contributors.js).
// 4. With snapshot_first, the doc's body as it was just before the change is
//    returned, taken in the same step, so the version the API stores from it is
//    exactly what the change replaced.
// 5. With agent_edit, the range the agent wrote is recorded beside the body (the
//    doc's `agentEdits` map, below), for the web app to mark and offer Revert.
// 6. The doc is stored at once (the direct connection's transact saves), and the
//    answer says whether the store succeeded: a save that failed is retried by
//    saveFailures.js, and the answer says `persisted: false` until then.

import { randomUUID } from 'node:crypto'
import * as Y from 'yjs'
import { TiptapTransformer } from '@hocuspocus/transformer'
import { bodyHash } from '../../docState.js'
import { epochIn } from '../../lineage.js'
import { renderDocHTML } from '../../preserved.js'
import { FIELD } from './blocks.js'

// The origin of every op's transaction, so it can be told apart from an edit
// that came over a socket.
export const LIVE_ORIGIN = Object.freeze({ liveDocWriter: true })

// AGENT EDITS. Where an agent wrote, beside the body and not in it: a Y.Map of
// edit id -> { agent, asker, run, at, from, to }, from and to being base64
// Y.RelativePositions (Y.encodeRelativePosition) of the first block written and
// the end of the last, on the `default` fragment. Not a mark in the text: a mark
// the editor's schema does not know makes an editor delete the whole text it
// sits in (y-prosemirror), so a new mark ships only with the web's spec (I4).
// The web app draws these ranges as decorations (B9's agent-edit extension); a
// run's Revert restores the agent_run version the API stored before it.
export const AGENT_EDITS = 'agentEdits'
export const MAX_AGENT_EDITS = 100

export const MAX_BLOCK_IDS = 500

const str = (v) => (typeof v === 'string' ? v.trim() : '')
const b64 = (u8) => Buffer.from(u8).toString('base64')

// refuse is an error the live server answers with its status (live/server.js).
export const refuse = (status, msg) => Object.assign(new Error(msg), { status })

// writeOptions is what every write carries.
export function writeOptions(body) {
    const by = body?.by && typeof body.by === 'object' ? body.by : {}
    const user = str(by.user_uuid)
    const agent = str(by.agent_uuid)
    return {
        user,
        agent,
        agentEdit: body?.agent_edit === true && agent !== '',
        snapshotFirst: body?.snapshot_first === true,
        run: str(body?.run_id),
    }
}

// htmlOf is the request's HTML; an op that cannot do without it refuses.
export function htmlOf(body, { required = true } = {}) {
    const html = typeof body?.html === 'string' ? body.html.trim() : ''
    if (!html && required) throw refuse(400, 'html is required')
    return html
}

// currentHTML is a doc's body as HTML now, '' when it holds nothing.
export function currentHTML(doc, extensions) {
    const json = TiptapTransformer.fromYdoc(doc, FIELD)
    if (!json || !Array.isArray(json.content) || json.content.length === 0) return ''
    return renderDocHTML(json, extensions).html
}

// openDoc is a direct connection to the doc as `who`. A doc that cannot be read
// is refused: 404 for no such doc, 410 for one in the trash (it takes no
// writes), else 503 (the API or the graph did not answer; the caller may try
// again).
export async function openDoc(hocuspocus, id, who = {}) {
    try {
        return await hocuspocus.openDirectConnection(id, {
            user: { id: who.user || who.agent || 'unknown' },
            liveDocWriter: true,
        })
    } catch (e) {
        if (e?.response?.status === 404) throw refuse(404, 'no such doc')
        if (e?.response?.status === 410) throw refuse(410, 'the doc is in the trash')
        throw refuse(503, 'the doc could not be opened just now; try again shortly')
    }
}

// recordAgentEdit notes the blocks [from, to) as an agent's edit.
function recordAgentEdit(doc, o, from, to) {
    const map = doc.getMap(AGENT_EDITS)
    const fragment = doc.getXmlFragment(FIELD)
    const id = randomUUID()
    map.set(id, {
        agent: o.agent,
        asker: o.user,
        run: o.run,
        at: Date.now(),
        from: b64(Y.encodeRelativePosition(Y.createRelativePositionFromTypeIndex(fragment, from))),
        to: b64(Y.encodeRelativePosition(Y.createRelativePositionFromTypeIndex(fragment, to, -1))),
    })
    if (map.size > MAX_AGENT_EDITS) {
        const oldest = [...map.entries()].sort((a, b) => (a[1]?.at || 0) - (b[1]?.at || 0))
        for (const [key] of oldest.slice(0, map.size - MAX_AGENT_EDITS)) map.delete(key)
    }
    return id
}

// runWrite runs one write.
//   build()               turns the request into blocks, before the doc is
//                         touched (it may refuse);
//   locate(doc, built)    finds where they go and checks the op may run; it
//                         changes nothing and may refuse;
//   apply(doc, built, at) makes the change and returns
//                         { from, to, blocks, removed, blockIds }: [from, to)
//                         the top-level blocks it wrote. Any other field it
//                         returns is the op's own (applyProposal's `applied`)
//                         and comes back in the answer as it is.
export async function runWrite({ docName, body, hocuspocus, deps }, { build, locate, apply }) {
    const id = docName.toLowerCase()
    // A doc that went to the trash (or was archived) while open is let go:
    // nothing written into it now could be saved (saveFailures.js).
    if (deps.isGone?.(id)) throw refuse(410, 'the doc is in the trash')
    const o = writeOptions(body)
    const built = build()
    const connection = await openDoc(hocuspocus, id, o)
    let result = null
    let before = null
    let stateVector = null
    let lineage = ''
    try {
        // Refused before anyone is credited with a change: the check runs again
        // with the write, in case the doc moved while the credit was recorded.
        locate(connection.document, built)
        await deps.contributors.record(id, { user: o.user, agent: o.agent })
        await connection.transact((doc) => {
            const at = locate(doc, built)
            if (o.snapshotFirst) before = { html: currentHTML(doc, deps.extensions), lineage: epochIn(doc) || '' }
            doc.transact(() => {
                result = apply(doc, built, at)
                if (o.agentEdit && result.to > result.from) result.editId = recordAgentEdit(doc, o, result.from, result.to)
            }, LIVE_ORIGIN)
            stateVector = Y.encodeStateVector(doc)
            lineage = epochIn(doc) || ''
        })
    } finally {
        await connection.disconnect()
    }
    const stored = deps.persistence.covers(id, stateVector) ? deps.persistence.get(id) : null
    if (!stored) deps.log?.error?.(`[Live] ${id}: the write is in the live doc, but its save has not succeeded yet; it is being retried`)
    // The op's own fields (applyProposal's `applied`), then the write's.
    const own = { ...result }
    for (const k of ['from', 'to', 'blocks', 'removed', 'blockIds', 'editId']) delete own[k]
    return {
        ...own,
        blocks: result.blocks,
        removed: result.removed || 0,
        block_ids: result.blockIds || [],
        edit_id: result.editId || '',
        lineage,
        persisted: !!stored,
        persisted_at: stored ? new Date(stored.at).toISOString() : null,
        before: before && before.html ? { html: before.html, lineage: before.lineage, hash: bodyHash(before.html) } : null,
    }
}
