// The read-only blocks the web editor draws itself (a React node view): here
// only their attributes need to survive. Same name, attributes and HTML as the
// web's extensions/<name>/<name>.ts.
import { Node, mergeAttributes } from '@tiptap/core'

// A data-* attribute that round-trips a string, as the web editor writes it.
const dataAttr = (name, key, fallback = '') => ({
    default: fallback,
    parseHTML: el => el.getAttribute(key) || fallback,
    renderHTML: attrs => (attrs[name] ? { [key]: attrs[name] } : {}),
})

const embed = (name, dataType, attrs) => Node.create({
    name,
    group: 'block',
    atom: true,
    selectable: true,
    draggable: name === 'tableEmbed',
    addAttributes() {
        return Object.fromEntries(Object.entries(attrs).map(([attr, key]) => [attr, dataAttr(attr, key)]))
    },
    parseHTML() { return [{ tag: `div[data-type="${dataType}"]` }] },
    renderHTML({ HTMLAttributes }) {
        return ['div', mergeAttributes({ 'data-type': dataType }, HTMLAttributes)]
    },
})

// `/table`: a live view of a table (extensions/table-embed).
export const TableEmbedSchema = embed('tableEmbed', 'table-embed', { tableId: 'data-table-id', viewId: 'data-view-id' })
// A poll card (extensions/poll).
export const PollSchema = embed('poll', 'poll', { id: 'data-id' })
// An agent's chart, diff and meeting recording blocks.
export const ChartEmbedSchema = embed('chartEmbed', 'chart', { spec: 'data-spec' })
export const DiffEmbedSchema = embed('diffEmbed', 'diff', { diff: 'data-diff' })
export const RecordingEmbedSchema = embed('recordingEmbed', 'recording-embed', {
    egress: 'data-egress', kind: 'data-kind', sid: 'data-sid', u1: 'data-u1', u2: 'data-u2',
})
