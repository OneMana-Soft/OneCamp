// A toggle (the web's extensions/collapsible, docs programme B2): its first
// line is its summary, a paragraph or a heading, and the blocks after it fold
// away. Open or closed is each reader's own and never in the doc.
//
// An old toggle kept its title in a `title` attribute and a shared `open`.
// Saved HTML with data-title reads as a first line and its blocks (here, as
// on the web); an old Yjs state moves over as the doc loads
// (transforms/legacyContent.js).
import { Node, mergeAttributes } from '@tiptap/core'
import { DOMParser, Fragment } from '@tiptap/pm/model'

// An old toggle (data-title, with its data-open): its title as its first
// line, or an empty one, so its blocks stay its blocks.
function legacyContent(el, schema) {
    const body = DOMParser.fromSchema(schema).parseSlice(el).content
    const title = el.getAttribute('data-title')
    if (title === null && !el.hasAttribute('data-open')) return body
    return Fragment.from(schema.nodes.paragraph.create(null, title ? schema.text(title) : null)).append(body)
}

// A <details> from elsewhere: its <summary> as the first line, a heading when
// it holds one (a toggle heading).
function detailsContent(el, schema) {
    const parser = DOMParser.fromSchema(schema)
    const summary = Array.from(el.children || []).find((c) => c.nodeName === 'SUMMARY')
    // Parsed as a doc, so loose text is a paragraph and a heading stays one.
    const head = summary ? parser.parse(summary).content : Fragment.empty
    const lead = head.firstChild?.type.name
    const first = lead === 'paragraph' || lead === 'heading' ? head : Fragment.from(schema.nodes.paragraph.create()).append(head)
    const rest = el.cloneNode(true)
    const restSummary = Array.from(rest.children || []).find((c) => c.nodeName === 'SUMMARY')
    if (restSummary) rest.removeChild(restSummary)
    return first.append(parser.parseSlice(rest).content)
}

export const CollapsibleSchema = Node.create({
    name: 'collapsible',
    group: 'block',
    content: '(paragraph | heading) block*',
    defining: true,
    isolating: true,
    parseHTML() {
        return [
            { tag: 'div[data-type="collapsible"]', getContent: legacyContent },
            { tag: 'details', getContent: detailsContent },
        ]
    },
    renderHTML({ HTMLAttributes }) {
        return ['div', mergeAttributes({ 'data-type': 'collapsible' }, HTMLAttributes), 0]
    },
})
