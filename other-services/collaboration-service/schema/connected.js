// Connected work (docs programme C1): what ties a doc to other pages, tasks,
// projects and dates. Same names, attributes and HTML as the web's
// extensions/{subpage,date-mention,task-list-embed,board-embed,task-item-link},
// so a saved body is what the web writes. Each holds only ids and a date;
// what they show (a page's title, a task's status, a project's tasks) is read
// live when drawn, and the HTML keeps a plain link or the date, for anything
// that reads the body without the app (an export, a search).
import { Extension, Node, mergeAttributes } from '@tiptap/core'

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
const ISO_DATE = /^(\d{4})-(\d{2})-(\d{2})$/
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']

// An id as the schema keeps it: a uuid in lower case, or null.
export const idOf = (v) => (typeof v === 'string' && UUID.test(v) ? v.toLowerCase() : null)
// A day on the calendar, written "2026-10-12"; "2026-02-30" is none.
const dateOf = (v) => {
    const m = typeof v === 'string' ? ISO_DATE.exec(v) : null
    if (!m) return null
    const d = new Date(0)
    d.setUTCFullYear(+m[1], +m[2] - 1, +m[3])
    return d.getUTCFullYear() === +m[1] && d.getUTCMonth() === +m[2] - 1 && d.getUTCDate() === +m[3] ? v : null
}

// The date as the body writes it, whatever the reader's language: "12 Oct
// 2026" (the codec's dateWords, rule for rule).
export function dateWords(iso) {
    const d = dateOf(iso)
    const m = d ? Number(d.slice(5, 7)) - 1 : -1
    if (m < 0 || m > 11) return ''
    return `${d.slice(8, 10).replace(/^0+/, '')} ${MONTHS[m]} ${d.slice(0, 4)}`
}

// An id kept as data-<key>, dropped unless it is a uuid.
const idAttr = (name, key) => ({
    default: null,
    parseHTML: (el) => idOf(el.getAttribute(key)),
    renderHTML: (attrs) => (idOf(attrs[name]) ? { [key]: idOf(attrs[name]) } : {}),
})

const textAttr = (name, key) => ({
    default: null,
    parseHTML: (el) => el.getAttribute(key) || null,
    renderHTML: (attrs) => (attrs[name] ? { [key]: String(attrs[name]) } : {}),
})

const linked = (href, words) => (href ? [['a', { href }, words]] : [])

// A page in the doc: a child page made with /page, or a link to any page made
// with /link to page. It keeps the page's id only (a title kept by an older
// client is still read); the app draws the page's title and icon as they are,
// for whoever may open it, and the reader without the app reads "Page".
export const SubpageSchema = Node.create({
    name: 'subpage',
    group: 'block',
    atom: true,
    selectable: true,
    draggable: false,
    addAttributes() {
        return { docId: idAttr('docId', 'data-doc-id'), title: textAttr('title', 'data-title') }
    },
    parseHTML() { return [{ tag: 'div[data-type="subpage"]' }] },
    renderHTML({ node, HTMLAttributes }) {
        const id = idOf(node.attrs.docId)
        return ['div', mergeAttributes({ 'data-type': 'subpage' }, HTMLAttributes), ...linked(id && `/app/doc/${id}`, node.attrs.title || 'Page')]
    },
})

// A date in a line: @today, @next monday, @12 Oct. Kept as the day it names
// (an ISO date), so "Today" reads "Yesterday" tomorrow. With a reminder, the
// id of the Later item it set.
export const DateMentionSchema = Node.create({
    name: 'dateMention',
    group: 'inline',
    inline: true,
    atom: true,
    selectable: true,
    addAttributes() {
        return {
            date: {
                default: null,
                parseHTML: (el) => dateOf(el.getAttribute('datetime')),
                renderHTML: (attrs) => (dateOf(attrs.date) ? { datetime: attrs.date } : {}),
            },
            reminder: textAttr('reminder', 'data-reminder'),
        }
    },
    parseHTML() { return [{ tag: 'time[data-type="date-mention"]' }] },
    renderHTML({ node, HTMLAttributes }) {
        return ['time', mergeAttributes({ 'data-type': 'date-mention' }, HTMLAttributes), dateWords(node.attrs.date)]
    },
    renderText({ node }) { return dateWords(node.attrs.date) },
})

const embed = (name, dataType, words, extra = {}) => Node.create({
    name,
    group: 'block',
    atom: true,
    selectable: true,
    draggable: false,
    addAttributes() {
        return { projectId: idAttr('projectId', 'data-project-id'), ...extra }
    },
    parseHTML() { return [{ tag: `div[data-type="${dataType}"]` }] },
    renderHTML({ node, HTMLAttributes }) {
        const id = idOf(node.attrs.projectId)
        return ['div', mergeAttributes({ 'data-type': dataType }, HTMLAttributes), ...linked(id && `/app/project/${id}`, words)]
    },
})

export const TASK_LIST_SHOWS = ['open', 'mine', 'all']

// /tasks: a project's tasks, live; ticking one completes it.
export const TaskListEmbedSchema = embed('taskListEmbed', 'task-list-embed', 'Tasks', {
    show: {
        default: 'open',
        parseHTML: (el) => (TASK_LIST_SHOWS.includes(el.getAttribute('data-show')) ? el.getAttribute('data-show') : 'open'),
        renderHTML: (attrs) => ({ 'data-show': TASK_LIST_SHOWS.includes(attrs.show) ? attrs.show : 'open' }),
    },
})

// A project's board, live.
export const BoardEmbedSchema = embed('boardEmbed', 'board-embed', 'Board')

// A to-do that became a task (⌘⇧Enter): its task's id on the to-do.
export const TaskItemLinkSchema = Extension.create({
    name: 'taskItemLink',
    addGlobalAttributes() {
        return [{ types: ['taskItem'], attributes: { taskId: { ...idAttr('taskId', 'data-task-id'), keepOnSplit: false } } }]
    },
})
