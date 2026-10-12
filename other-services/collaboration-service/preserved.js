// Writing a doc's HTML body without ever losing the doc.
//
// WHY THIS EXISTS. The body is written from the Yjs document with this
// service's schema (extensions.js). A node or mark the schema does not know
// made generateHTML throw ("Unknown node type: tableEmbed"). Until 9 Oct the
// save caught that and wrote "<p>Error generating HTML</p>" over the whole doc;
// after it, the save threw on every try, so the doc stopped saving at all.
//
// schemaParity.test.js keeps the schema equal to the web editor's, so this
// should not happen. When it does anyway (a block added on the web first, an
// older server meeting a newer editor), renderDocHTML still writes the body:
//
//   1. as normal;
//   2. else with every unknown node and mark kept as it is: same name, same
//      attributes, written as <div|span data-oc-preserved="name"> with the
//      attributes as JSON, and its content inside;
//   3. else block by block, a block that still cannot be written kept as its
//      text (when no block at all can be written, it throws RenderError, and
//      the caller keeps the stored body).
//
// htmlToDoc reads such a body back to the same nodes and marks, under their own
// names, so a doc rebuilt from its body (no saved Yjs state) gets them back and
// the web editor, which knows them, draws them as before.

import { Mark, Node, getSchema } from '@tiptap/core'
import { generateHTML, generateJSON } from '@tiptap/html'

const NAME = /^[A-Za-z][\w-]{0,63}$/
const ALIAS = 'ocPreserved__'

export class RenderError extends Error {}

// ---------------------------------------------------------------------------
// Finding what the schema does not know
// ---------------------------------------------------------------------------

// unknownTypes walks a doc's JSON and returns the node and mark types that
// schema lacks: for a node, whether it sits in running text (inline), what it
// holds ('inline', 'block' or 'none') and the attribute names seen on it.
export function unknownTypes(json, schema) {
    const nodes = new Map()
    const marks = new Map()
    const note = (map, name, info, attrs) => {
        const seen = map.get(name) || { ...info, attrs: new Set() }
        for (const k of Object.keys(attrs || {})) seen.attrs.add(k)
        map.set(name, seen)
    }
    const isInline = (child) => {
        if (!child || typeof child !== 'object') return false
        if (child.type === 'text') return true
        const known = schema.nodes[child.type]
        return known ? known.isInline : false
    }
    const visit = (node, inlineContext) => {
        if (!node || typeof node !== 'object') return
        for (const m of node.marks || []) {
            if (m && typeof m.type === 'string' && !schema.marks[m.type]) note(marks, m.type, {}, m.attrs)
        }
        const children = Array.isArray(node.content) ? node.content : null
        const known = schema.nodes[node.type]
        let childrenInline
        if (known) {
            childrenInline = known.inlineContent
        } else {
            const holds = children && children.length ? (children.some(isInline) ? 'inline' : 'block') : 'none'
            if (typeof node.type === 'string') note(nodes, node.type, { inline: inlineContext, holds }, node.attrs)
            childrenInline = holds === 'inline'
        }
        for (const c of children || []) visit(c, childrenInline)
    }
    visit(json, false)
    return { nodes, marks }
}

// ---------------------------------------------------------------------------
// Generic "keep it as it is" nodes and marks
// ---------------------------------------------------------------------------

const attrsOf = (names, blobAttr) => () =>
    Object.fromEntries([...names].map((k) => [k, {
        default: null,
        // Read from the JSON blob when parsing; written as part of it.
        parseHTML: (el) => {
            try { return JSON.parse(el.getAttribute('data-oc-attrs') || '{}')[k] ?? null } catch { return null }
        },
        renderHTML: () => ({}),
    }]).concat(blobAttr ? [[blobAttr, {
        default: '{}',
        parseHTML: (el) => el.getAttribute('data-oc-attrs') || '{}',
        renderHTML: () => ({}),
    }]] : []))

const blobOf = (attrs) => {
    const out = {}
    for (const [k, v] of Object.entries(attrs || {})) if (v !== null && v !== undefined) out[k] = v
    return JSON.stringify(out)
}

// preservedNode is a node named `name` that writes itself as a marker element
// and reads a marker for `realName` back. `blobAttr`, when set, also keeps the
// marker's whole attribute JSON in one attribute (used while reading, when the
// names are not known yet).
export function preservedNode(name, realName, { inline, holds, attrs = [] }, blobAttr) {
    const tag = inline ? 'span' : 'div'
    return Node.create({
        name,
        inline: !!inline,
        group: inline ? 'inline' : 'block',
        atom: holds === 'none',
        content: holds === 'inline' ? 'inline*' : holds === 'block' ? 'block*' : undefined,
        addAttributes: attrsOf(attrs, blobAttr),
        parseHTML() {
            return [{ tag: `${tag}[data-oc-preserved="${realName}"]`, priority: 1000 }]
        },
        renderHTML({ node }) {
            const own = { ...node.attrs }
            if (blobAttr) delete own[blobAttr]
            const el = [tag, { 'data-oc-preserved': realName, 'data-oc-holds': holds, 'data-oc-attrs': blobOf(own) }]
            return holds === 'none' ? el : [...el, 0]
        },
    })
}

export function preservedMark(name, realName, { attrs = [] }, blobAttr) {
    return Mark.create({
        name,
        addAttributes: attrsOf(attrs, blobAttr),
        parseHTML() {
            return [{ tag: `span[data-oc-mark="${realName}"]`, priority: 1000 }]
        },
        renderHTML({ mark }) {
            const own = { ...mark.attrs }
            if (blobAttr) delete own[blobAttr]
            return ['span', { 'data-oc-mark': realName, 'data-oc-attrs': blobOf(own) }, 0]
        },
    })
}

// keepersFor is the extensions that keep each unknown type under its own name.
export function keepersFor({ nodes, marks }) {
    const out = []
    for (const [name, info] of nodes) if (NAME.test(name)) out.push(preservedNode(name, name, { ...info, attrs: info.attrs }))
    for (const [name, info] of marks) if (NAME.test(name)) out.push(preservedMark(name, name, { attrs: info.attrs }))
    return out
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

const escapeHTML = (s) => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;')

const textOf = (node) => {
    if (!node || typeof node !== 'object') return ''
    if (node.type === 'text') return node.text || ''
    return (node.content || []).map(textOf).join(node.content?.some((c) => c?.type !== 'text') ? ' ' : '')
}

// renderDocHTML is the doc's HTML body, never a placeholder. It returns
// { html, preserved, degraded }: `preserved` names the node and mark types kept
// by a marker, `degraded` the number of top-level blocks kept only as text. It
// throws RenderError only when not even that can be written; the caller then
// keeps the stored body.
export function renderDocHTML(json, extensions, render = generateHTML) {
    let first
    try {
        return { html: render(json, extensions), preserved: [], degraded: 0 }
    } catch (e) {
        first = e
    }

    let exts = extensions
    let preserved = []
    try {
        const unknown = unknownTypes(json, getSchema(extensions))
        preserved = [...unknown.nodes.keys(), ...unknown.marks.keys()]
        if (preserved.length) {
            exts = [...extensions, ...keepersFor(unknown)]
            return { html: render(json, exts), preserved, degraded: 0 }
        }
    } catch (e) {
        // fall through to block by block
    }

    try {
        let degraded = 0
        const blocks = (Array.isArray(json?.content) ? json.content : []).map((block) => {
            try {
                return render({ type: 'doc', content: [block] }, exts)
            } catch (e) {
                degraded++
                return `<p data-oc-unrendered="${escapeHTML(block?.type || '')}">${escapeHTML(textOf(block))}</p>`
            }
        })
        // Not one block could be written: that is a failure, not a body.
        if (blocks.length === 0 || degraded === blocks.length) throw first
        return { html: blocks.join(''), preserved, degraded }
    } catch (e) {
        throw new RenderError(`the body could not be written: ${first?.message || first}`)
    }
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

const MARKER = /<(div|span)\b[^>]*?\bdata-oc-(preserved|mark)="([^"]+)"[^>]*>/g
const HOLDS = /\bdata-oc-holds="(inline|block|none)"/

const rename = (json, aliases) => {
    const fix = (item) => {
        const real = aliases.get(item.type)
        if (!real) return item
        let attrs = {}
        try { attrs = JSON.parse(item.attrs?.__ocAttrs || '{}') } catch { attrs = {} }
        return { ...item, type: real, attrs }
    }
    const visit = (node) => {
        if (!node || typeof node !== 'object') return node
        const out = fix(node)
        if (out.marks) out.marks = out.marks.map(fix)
        if (out.content) out.content = out.content.map(visit)
        return out
    }
    return visit(json)
}

// The server's DOM (happy-dom, under @tiptap/html) decodes only &quot; and
// &amp; inside attribute values, and the writer encodes a newline as "&#xA;"
// and "<" as "&lt;": a diff block read back from its body came back with
// "&#xA;" for every line break. So the other references in attribute values
// are decoded before parsing (a raw "<" or line break is valid inside a quoted
// value), and a decoded '"' or '&' is put back as &quot; / &amp;.
const NAMED = { lt: '<', gt: '>', apos: "'", nbsp: ' ' }
const TAG = /<[A-Za-z][\w:-]*(?:\s+[^\s"'=<>`/]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'=<>`]+))?)*\s*\/?>/g
const REF = /&(#[xX][0-9a-fA-F]{1,6}|#\d{1,7}|[A-Za-z]{2,8});/g

function decodeRef(whole, ref) {
    let ch
    if (ref[0] === '#') {
        const code = ref[1] === 'x' || ref[1] === 'X' ? parseInt(ref.slice(2), 16) : parseInt(ref.slice(1), 10)
        if (!Number.isFinite(code) || code <= 0 || code > 0x10ffff || (code >= 0xd800 && code <= 0xdfff)) return whole
        ch = String.fromCodePoint(code)
    } else {
        ch = NAMED[ref]
        if (ch === undefined) return whole
    }
    if (ch === '"') return '&quot;'
    if (ch === '&') return '&amp;'
    return ch
}

export function decodeAttributeRefs(html) {
    if (typeof html !== 'string' || !html.includes('&')) return html
    return html.replace(TAG, (tag) => tag.replace(/"([^"]*)"/g, (_, value) => `"${value.replace(REF, decodeRef)}"`))
}

// htmlToDoc reads a stored body into the doc's JSON. A body holding kept
// markers comes back with those nodes and marks under their own names, and
// `extensions` then also holds a keeper for each one the schema still lacks, so
// the JSON can be turned into a Yjs document (TiptapTransformer.toYdoc) with
// them in it.
export function htmlToDoc(html, extensions) {
    html = decodeAttributeRefs(html)
    if (typeof html !== 'string' || !html.includes('data-oc-')) {
        return { json: generateJSON(html, extensions), extensions }
    }
    const found = { nodes: new Map(), marks: new Map() }
    for (const m of html.matchAll(MARKER)) {
        const [tag, el, kind, name] = m
        if (!NAME.test(name)) continue
        if (kind === 'mark') found.marks.set(name, {})
        else found.nodes.set(name, { inline: el === 'span', holds: (tag.match(HOLDS) || [])[1] || 'none' })
    }
    if (found.nodes.size === 0 && found.marks.size === 0) {
        return { json: generateJSON(html, extensions), extensions }
    }
    // Read every marker into an alias carrying its attributes as JSON, then
    // give each its own name back.
    const aliases = new Map()
    const readers = []
    for (const [name, info] of found.nodes) {
        aliases.set(ALIAS + name, name)
        readers.push(preservedNode(ALIAS + name, name, info, '__ocAttrs'))
    }
    for (const [name] of found.marks) {
        aliases.set(ALIAS + name, name)
        readers.push(preservedMark(ALIAS + name, name, {}, '__ocAttrs'))
    }
    const json = rename(generateJSON(html, [...extensions, ...readers]), aliases)
    const unknown = unknownTypes(json, getSchema(extensions))
    for (const [name, info] of unknown.nodes) {
        const seen = found.nodes.get(name)
        if (seen) Object.assign(info, { inline: seen.inline, holds: seen.holds })
    }
    return { json, extensions: [...extensions, ...keepersFor(unknown)] }
}
