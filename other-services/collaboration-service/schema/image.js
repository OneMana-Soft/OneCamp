// An image: the web's (extensions/image/image.ts) is an atom with an id, a
// caption and the uploaded file's name besides Tiptap's own attributes, and
// its alignment, crop and link as data-* (docs programme B4).
import { Image } from '@tiptap/extension-image'

const dataAttr = (key) => ({
    default: null,
    parseHTML: (el) => el.getAttribute(`data-${key}`),
    renderHTML: (a) => (a[key] ? { [`data-${key}`]: a[key] } : {}),
})

export const ImageSchema = Image.extend({
    atom: true,
    addAttributes() {
        return {
            ...this.parent?.(),
            id: { default: null },
            caption: { default: null },
            fileName: { default: null },
            align: dataAttr('align'),
            crop: dataAttr('crop'),
            href: dataAttr('href'),
        }
    },
})
