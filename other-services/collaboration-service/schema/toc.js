// A table of contents (docs programme B2; the web's extensions/toc): the doc
// stores only that it is here, and each reader's editor lists the headings.
import { Node, mergeAttributes } from '@tiptap/core'

export const TocSchema = Node.create({
    name: 'toc',
    group: 'block',
    atom: true,
    selectable: true,
    draggable: true,
    parseHTML() {
        return [{ tag: 'div[data-type="toc"]' }, { tag: 'nav[data-type="toc"]' }]
    },
    renderHTML({ HTMLAttributes }) {
        return ['div', mergeAttributes({ 'data-type': 'toc' }, HTMLAttributes)]
    },
})
