// A large quote (docs programme B2; the web's extensions/quote-size):
// quoteSize on a doc's quotes, written data-quote-size="large".
import { Extension } from '@tiptap/core'

export const QuoteSizeSchema = Extension.create({
    name: 'quoteSize',
    addGlobalAttributes() {
        return [
            {
                types: ['blockquote'],
                attributes: {
                    quoteSize: {
                        default: null,
                        parseHTML: (el) => (el.getAttribute('data-quote-size') === 'large' ? 'large' : null),
                        renderHTML: (attrs) => (attrs.quoteSize === 'large' ? { 'data-quote-size': 'large' } : {}),
                    },
                },
            },
        ]
    },
})
