// The code block. The web highlights it in the browser (lazily); here the
// same node, with the language attribute, and a doc's two more (docs
// programme B3): wrap (data-wrap="true") and a caption (data-caption), on its
// <pre>, as the web's extensions/code-block-lowlight/docCodeBlock.ts writes them.
import { CodeBlockLowlight } from '@tiptap/extension-code-block-lowlight'
import { common, createLowlight } from 'lowlight'

export const CodeBlockSchema = CodeBlockLowlight.extend({
    addAttributes() {
        return {
            ...this.parent?.(),
            wrap: {
                default: false,
                parseHTML: (el) => el.getAttribute('data-wrap') === 'true',
                renderHTML: (attrs) => (attrs.wrap ? { 'data-wrap': 'true' } : {}),
            },
            caption: {
                default: null,
                parseHTML: (el) => el.getAttribute('data-caption') || null,
                renderHTML: (attrs) => (attrs.caption ? { 'data-caption': attrs.caption } : {}),
            },
        }
    },
}).configure({ lowlight: createLowlight(common) })
