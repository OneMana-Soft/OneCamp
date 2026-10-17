// Block identity (docs programme §4 #4, A1): every block carries a stable id,
// `blockId`, written as data-block-id. Threads (A8), edit proposals (A9), the
// Live Doc Writer's insertAfter and replaceBlocks (A4) and links to a block
// find a block by it.
//
// Which nodes: every node that isn't inline, except the doc itself: the same
// rule as the web's extensions/block-id/blockId.ts, and the schema manifest
// holds the two to the same attributes. The server never makes ids as people
// type (the browser does, in its own transactions only); it gives a doc that
// has none its ids once, as it loads (transforms/blockIds.js).
import { Extension, getExtensionField } from '@tiptap/core'

export const BLOCK_ID_ATTR = 'blockId'

const NOT_BLOCKS = new Set(['doc', 'text'])

// blockTypesOf is the node extensions that are blocks: not inline, not the doc.
export function blockTypesOf(extensions) {
    return extensions
        .filter((e) => e.type === 'node' && !NOT_BLOCKS.has(e.name))
        .filter((e) => {
            const ctx = { name: e.name, options: e.options, storage: e.storage }
            const inline = getExtensionField(e, 'inline', ctx)
            const isInline = typeof inline === 'function' ? inline() : inline
            const group = getExtensionField(e, 'group', ctx)
            const groups = typeof group === 'function' ? group() : group
            return !isInline && !(groups ?? '').split(' ').includes('inline')
        })
        .map((e) => e.name)
}

export const BlockIdSchema = Extension.create({
    name: 'blockId',
    addGlobalAttributes() {
        return [
            {
                types: blockTypesOf(this.extensions ?? []),
                attributes: {
                    [BLOCK_ID_ATTR]: {
                        default: null,
                        keepOnSplit: false,
                        parseHTML: (el) => el.getAttribute('data-block-id') || null,
                        renderHTML: (attrs) => (attrs[BLOCK_ID_ATTR] ? { 'data-block-id': attrs[BLOCK_ID_ATTR] } : {}),
                    },
                },
            },
        ]
    },
})
