// What a link or a file becomes in a doc (docs programme B4): a bookmark, an
// embed, a file block and a link mention. Same names, attributes and HTML as
// the web's extensions/{bookmark,web-embed,file-block}/ and link/linkMention,
// so a saved body is what the web writes. Each block holds a plain link to
// what it points at (the web's lib/codec/mediaLink.ts).
import { Node, mergeAttributes } from '@tiptap/core'

// An absolute web address as written (no space around it), or one of the
// app's own paths for a file. The web's isSafeHref is always true for these
// shapes, so the shape alone gives the same bytes.
const webHref = (s) => typeof s === 'string' && /^https?:\/\/\S/i.test(s) && !/\s$/.test(s)
const fileHref = (s) => webHref(s) || (typeof s === 'string' && /^\/[^/\\\s]/.test(s) && !/\s$/.test(s))
const str = (v) => (typeof v === 'string' ? v : typeof v === 'number' && Number.isFinite(v) ? String(v) : '')

const FILE_KINDS = ['file', 'video', 'audio', 'pdf']

// mediaLinkOf is the address and the words of the link a node stands for.
export function mediaLinkOf(type, attrs) {
    const a = attrs || {}
    const file = type === 'fileBlock'
    const href = str(file || type === 'webEmbed' ? a.src : a.href)
    if (!href || !(file ? fileHref(href) : webHref(href))) return null
    const text = file ? str(a.name) || href : type === 'webEmbed' ? href : str(a.title) || href
    return { href, text }
}

const linkChildren = (type, attrs) => {
    const link = mediaLinkOf(type, attrs)
    return link ? [['a', { href: link.href }, link.text]] : []
}

// An attribute kept as data-<key>: a string, or with int a whole number.
const dataAttr = (name, key, int = false) => ({
    default: null,
    parseHTML: (el) => {
        const v = el.getAttribute(key)
        if (!v) return null
        return int ? (/^\d+$/.test(v) ? parseInt(v, 10) : null) : v
    },
    renderHTML: (attrs) => (attrs[name] === null || attrs[name] === undefined || attrs[name] === '' ? {} : { [key]: String(attrs[name]) }),
})

const block = (name, dataType, attrs) =>
    Node.create({
        name,
        group: 'block',
        atom: true,
        selectable: true,
        draggable: false,
        addAttributes: attrs,
        parseHTML() {
            return [{ tag: `div[data-type="${dataType}"]` }]
        },
        renderHTML({ node, HTMLAttributes }) {
            return ['div', mergeAttributes({ 'data-type': dataType }, HTMLAttributes), ...linkChildren(name, node.attrs)]
        },
    })

// A bookmark: a link as a card, its preview as the server read it.
export const BookmarkSchema = block('bookmark', 'bookmark', () => ({
    href: dataAttr('href', 'data-href'),
    title: dataAttr('title', 'data-title'),
    description: dataAttr('description', 'data-description'),
    siteName: dataAttr('siteName', 'data-site'),
    icon: dataAttr('icon', 'data-icon'),
    image: dataAttr('image', 'data-image'),
}))

// An embed: the link someone pasted (what is framed is made from it when drawn).
export const WebEmbedSchema = block('webEmbed', 'web-embed', () => ({
    src: dataAttr('src', 'data-src'),
    height: dataAttr('height', 'data-height', true),
}))

// A file: a video, audio, a PDF or any file kept with the doc.
export const FileBlockSchema = block('fileBlock', 'file', () => ({
    kind: {
        default: 'file',
        parseHTML: (el) => {
            const k = el.getAttribute('data-kind') || ''
            return FILE_KINDS.includes(k) ? k : 'file'
        },
        renderHTML: (attrs) => ({ 'data-kind': FILE_KINDS.includes(attrs.kind) ? attrs.kind : 'file' }),
    },
    src: dataAttr('src', 'data-src'),
    id: dataAttr('id', 'data-id'),
    name: dataAttr('name', 'data-name'),
    size: dataAttr('size', 'data-size', true),
    mime: dataAttr('mime', 'data-mime'),
    caption: dataAttr('caption', 'data-caption'),
}))

// A link mention: the link itself, drawn as a chip; read ahead of the link mark.
export const LinkMentionSchema = Node.create({
    name: 'linkMention',
    group: 'inline',
    inline: true,
    atom: true,
    selectable: true,
    addAttributes() {
        return {
            href: {
                default: null,
                parseHTML: (el) => el.getAttribute('href'),
                renderHTML: (attrs) => {
                    const link = mediaLinkOf('linkMention', attrs)
                    return link ? { href: link.href } : {}
                },
            },
            title: dataAttr('title', 'data-title'),
            icon: dataAttr('icon', 'data-icon'),
            siteName: dataAttr('siteName', 'data-site'),
        }
    },
    parseHTML() {
        return [{ tag: 'a[data-type="link-mention"]', priority: 60 }]
    },
    renderHTML({ node, HTMLAttributes }) {
        const text = mediaLinkOf('linkMention', node.attrs)?.text ?? (node.attrs.title || '')
        return ['a', mergeAttributes({ 'data-type': 'link-mention' }, HTMLAttributes), text]
    },
})
