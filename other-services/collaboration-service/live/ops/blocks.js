// Blocks: turning HTML into top-level Yjs nodes, finding a block by its id, and
// telling two blocks apart, for the Live Doc Writer's ops.

import { randomUUID } from 'node:crypto'
import { getSchema } from '@tiptap/core'
import { TiptapTransformer } from '@hocuspocus/transformer'
import * as Y from 'yjs'
import { htmlToDoc } from '../../preserved.js'

export const FIELD = 'default'

// BLOCK_ID_ATTR is the attribute a block's stable id lives in (Block Identity,
// docs programme §4 #4; A1 adds it to the schema, written as data-block-id).
// Until the schema has it, no block has an id: insertAfter and replaceBlocks
// answer "no such block" and new blocks are written without one.
export const BLOCK_ID_ATTR = 'blockId'

// htmlToBlocks turns HTML into top-level nodes ready to insert into a doc's
// body, built with the editor's own schema in a scratch document and then
// cloned (a Yjs type belongs to one document; a clone is the unattached copy
// another can take). Each comes with its key (blockKey) and its block id: a
// block the schema gives an id to and the HTML did not gets a new one.
export function htmlToBlocks(html, extensions) {
    const { json, extensions: withKept } = htmlToDoc(html, extensions)
    const scratch = TiptapTransformer.toYdoc(json, FIELD, withKept)
    const schema = getSchema(withKept)
    return scratch.getXmlFragment(FIELD).toArray().map((node) => {
        const key = blockKey(node)
        const clone = node.clone()
        let id = null
        if (node instanceof Y.XmlElement) {
            id = node.getAttribute(BLOCK_ID_ATTR) || null
            const takesId = schema.nodes[node.nodeName]?.spec?.attrs?.[BLOCK_ID_ATTR] !== undefined
            if (!id && takesId) {
                id = randomUUID()
                clone.setAttribute(BLOCK_ID_ATTR, id)
            }
        }
        return { node: clone, key, id }
    })
}

// stable is JSON with object keys sorted, so equal values print the same.
const stable = (v) => {
    if (Array.isArray(v)) return `[${v.map(stable).join(',')}]`
    if (v && typeof v === 'object') return `{${Object.keys(v).sort().map((k) => `${JSON.stringify(k)}:${stable(v[k])}`).join(',')}}`
    return JSON.stringify(v)
}

// blockKey is a block's content and attributes, its id left out: two blocks with
// the same key read the same. replaceAll keeps a block whose key is unchanged,
// so its Yjs identity (and every comment anchored in it) survives.
export function blockKey(node) {
    if (node instanceof Y.XmlText) return `t${stable(node.toDelta())}`
    if (node instanceof Y.XmlElement) {
        const attrs = { ...node.getAttributes() }
        delete attrs[BLOCK_ID_ATTR]
        return `e${node.nodeName}${stable(attrs)}[${node.toArray().map(blockKey).join(',')}]`
    }
    return 'x'
}

// indexOfBlock is the position of the top-level block with that id, or -1.
export function indexOfBlock(fragment, id) {
    if (typeof id !== 'string' || !id) return -1
    const blocks = fragment.toArray()
    for (let i = 0; i < blocks.length; i++) {
        const b = blocks[i]
        if (b instanceof Y.XmlElement && b.getAttribute(BLOCK_ID_ATTR) === id) return i
    }
    return -1
}

// isBlank is whether a body holds nothing anyone wrote: no blocks, or only
// empty paragraphs (an editor that opens an empty doc writes one).
export function isBlank(fragment) {
    return fragment.toArray().every((b) =>
        b instanceof Y.XmlElement && b.nodeName === 'paragraph' &&
        b.toArray().every((c) => c instanceof Y.XmlText && c.length === 0))
}
