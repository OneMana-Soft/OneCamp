// A block's outline indent (B1; the web's extensions/indent): 0–8, written as
// data-indent. The blocks after one that are indented deeper are its
// children. Changing it is an attribute change, so nobody's typing inside the
// block is lost.
import { Extension } from '@tiptap/core'

export const INDENT_NODES = ['blockquote', 'bulletList', 'callout', 'chartEmbed', 'codeBlock', 'collapsible', 'diffEmbed', 'heading', 'horizontalRule', 'image', 'orderedList', 'paragraph', 'poll', 'recordingEmbed', 'tableEmbed', 'taskList']

const MAX_INDENT = 8

export const IndentSchema = Extension.create({
    name: 'indent',
    addGlobalAttributes() {
        return [
            {
                types: INDENT_NODES,
                attributes: {
                    indent: {
                        default: 0,
                        keepOnSplit: true,
                        parseHTML: (el) => {
                            const n = parseInt(el.getAttribute('data-indent') ?? '', 10)
                            return Number.isFinite(n) ? Math.min(MAX_INDENT, Math.max(0, n)) : 0
                        },
                        renderHTML: (attrs) => (typeof attrs.indent === 'number' && attrs.indent > 0 ? { 'data-indent': String(attrs.indent) } : {}),
                    },
                },
            },
        ]
    },
})
