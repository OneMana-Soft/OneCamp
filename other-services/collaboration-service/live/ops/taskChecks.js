// setTaskChecks: a task was done or opened again somewhere else (its board,
// its panel, the API), so every to-do in this doc that stands for it ticks or
// clears its box (docs programme C1, business/Task/taskDocSync.go). A to-do
// stands for a task when its taskItem carries the task's id (taskId, written
// as data-task-id by the web's task-item-link extension).
//
// body.task_checks is { "<task uuid>": true | false }. One transaction, like
// every op; a doc whose boxes already say so is left untouched, and answers
// changed: 0. held is how many to-dos stand for the tasks at all: 0 tells the
// API the doc no longer holds them, so it stops following them.

import * as Y from 'yjs'
import { idOf } from '../../schema/connected.js'
import { FIELD } from './blocks.js'
import { refuse, runWrite } from './write.js'

export const TASK_ID_ATTR = 'taskId'
const MAX_TASKS = 100

// checksOf is the request's { task: checked }, refused when it says nothing
// usable.
export function checksOf(body) {
    const raw = body?.task_checks
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) throw refuse(400, 'task_checks is required')
    const out = new Map()
    for (const [key, checked] of Object.entries(raw)) {
        const id = idOf(key)
        if (!id || typeof checked !== 'boolean') throw refuse(400, 'task_checks takes task ids and true or false')
        out.set(id, checked)
    }
    if (out.size === 0 || out.size > MAX_TASKS) throw refuse(400, `task_checks takes 1 to ${MAX_TASKS} tasks`)
    return out
}

// setChecks walks the body for to-dos standing for a task in checks, and sets
// each box that differs. Returns how many to-dos stand for one (held), and how
// many of those changed.
export function setChecks(fragment, checks) {
    let changed = 0
    let held = 0
    const walk = (node) => {
        if (!(node instanceof Y.XmlElement) && !(node instanceof Y.XmlFragment)) return
        if (node instanceof Y.XmlElement && node.nodeName === 'taskItem') {
            const id = String(node.getAttribute(TASK_ID_ATTR) || '').toLowerCase()
            if (checks.has(id)) {
                held++
                if (Boolean(node.getAttribute('checked')) !== checks.get(id)) {
                    node.setAttribute('checked', checks.get(id))
                    changed++
                }
            }
        }
        for (const child of node.toArray()) walk(child)
    }
    walk(fragment)
    return { changed, held }
}

export async function setTaskChecks(ctx) {
    return runWrite(ctx, {
        build: () => checksOf(ctx.body),
        locate: () => 0,
        apply: (doc, checks) => ({ from: 0, to: 0, blocks: 0, ...setChecks(doc.getXmlFragment(FIELD), checks) }),
    })
}
