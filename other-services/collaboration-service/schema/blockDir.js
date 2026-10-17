// A block's text direction (B1; the web's extensions/block-dir): "ltr" or
// "rtl", written as dir; none (null) is from the block's own letters.
import { Extension } from '@tiptap/core'

export const DIR_NODES = ['blockquote', 'bulletList', 'callout', 'collapsible', 'heading', 'orderedList', 'paragraph', 'taskList']

export const BlockDirSchema = Extension.create({
    name: 'blockDir',
    addGlobalAttributes() {
        return [
            {
                types: DIR_NODES,
                attributes: {
                    dir: {
                        default: null,
                        keepOnSplit: true,
                        parseHTML: (el) => {
                            const d = el.getAttribute('dir')
                            return d === 'ltr' || d === 'rtl' ? d : null
                        },
                        renderHTML: (attrs) => (attrs.dir === 'ltr' || attrs.dir === 'rtl' ? { dir: attrs.dir } : {}),
                    },
                },
            },
        ]
    },
})
