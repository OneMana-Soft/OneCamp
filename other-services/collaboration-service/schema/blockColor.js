// A block's own colour (docs programme B2; the web's extensions/block-color):
// blockColor on the blocks that take one, "sky" for blue text or
// "sky_background" for a blue background, written data-block-color.
import { Extension } from '@tiptap/core'
import { BLOCK_COLOUR_NODES, blockColourValue } from './colours.js'

export const BlockColorSchema = Extension.create({
    name: 'blockColor',
    addGlobalAttributes() {
        return [
            {
                types: BLOCK_COLOUR_NODES,
                attributes: {
                    blockColor: {
                        default: null,
                        keepOnSplit: false,
                        parseHTML: (el) => blockColourValue(el.getAttribute('data-block-color')),
                        renderHTML: (attrs) => (blockColourValue(attrs.blockColor) ? { 'data-block-color': attrs.blockColor } : {}),
                    },
                },
            },
        ]
    },
})
