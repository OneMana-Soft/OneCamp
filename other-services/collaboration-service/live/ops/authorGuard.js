// Everyone writes as themselves (V-965). Whoever may write a board over the
// socket (its editors, a call's guest the call lets draw) could write any
// person into what names one, so a comment, a reply, a reaction, a note's
// maker, a vote or a facilitation round could show as someone else's. Each
// such map is kept here against its sockets' changes, as their context names
// the writer (context.user: the id their token was authorised as, the name the
// API gives them):
// - an entry naming someone else, or changing someone else's (taking it over,
//   their name, their words, a reaction that isn't one's own), is put back as
//   it was and logged (the document, the writer, the map and the field; never
//   the text);
// - one's own entry stands as written, with the writer's own name stamped on
//   it when the tab says another (the tab names them as it shows them);
// - a deletion names nobody, and stands (an editor tidies a board).
// A doc's record of where an agent wrote (write.js AGENT_EDITS) is this
// service's alone, as guestStrokes' and protect's records are a board's.
// Changes with no socket behind them (loading, this service's ops) stand.
//
// The maps (web components/board/boardComments.tsx, lib/board/authors.ts,
// useBoardVoting.ts, boardFacilitation.tsx):
// - comments: a pin's id -> { authorId, authorName, messages: [{ id, authorId,
//   authorName, body, reactions: { emoji: [user ids] } }], ... };
// - noteAuthors: a note's id -> { id, name }, its maker;
// - votes: `${round}:${user id}` -> what they voted for;
// - elements: a note held in private mode (miro-25) names its author in
//   customData.privateBy and privateName, shown on it to everyone until they
//   open it (web lib/board/privateRecord.ts, privateVeils.tsx);
// - facilitation: rounds { byId, byName, ... } and the timer { byName, ... }
//   (facilitationGuard.js, which also keeps them from guests).

import { newer, putBackSocketWrites } from './mapGuard.js'
import { AGENT_EDITS } from './write.js'

export const COMMENTS = 'comments'
export const NOTE_AUTHORS = 'noteAuthors'
export const VOTES = 'votes'
export const ELEMENTS = 'elements'

// The origin of this service's put-backs and stamps.
export const AUTHOR_ORIGIN = Object.freeze({ authorGuard: true })

const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v)

// writerOf is the person a socket writes as; the comment op (comment.js) names
// the person the API writes for by it too.
export const writerOf = (context) => ({ id: context.user?.id, name: context.user?.name })

// someoneElse is whether an id names a person other than who.
const someoneElse = (id, who) => id !== undefined && id !== '' && id !== who.id

// person checks one entry naming a person by its fields idF and nameF, as
// `who` changed it from `before` (undefined: new) to `after`. Answers
// { fault: field } when it names someone else or changes someone else's;
// else { value }, the entry with the writer's own id and name stamped on an
// entry of theirs (stamped: true when that changed it). An entry naming
// nobody by id is the writer's; one named only by name (a timer) takes the
// writer's name when its name changes.
function person(before, after, who, idF, nameF) {
    if (!isObject(after)) return { value: after }
    const was = isObject(before) ? before : {}
    if (after[idF] === undefined && was[idF] === undefined) {
        if (!(nameF in after) || after[nameF] === was[nameF]) return { value: after }
        return stamp(after, { [nameF]: who.name })
    }
    if (after[idF] !== was[idF] && (someoneElse(after[idF], who) || someoneElse(was[idF], who))) return { fault: idF }
    if (someoneElse(after[idF], who)) return after[nameF] === was[nameF] ? { value: after } : { fault: nameF }
    return stamp(after, { [idF]: who.id, [nameF]: who.name })
}

function stamp(value, fields) {
    const due = Object.entries(fields).filter(([k, v]) => typeof v === 'string' && v !== '' && value[k] !== v)
    return due.length === 0 ? { value } : { value: { ...value, ...Object.fromEntries(due) }, stamped: true }
}

// reactionsShaped is whether a message's reactions are what the web reads:
// none, or emoji -> [user ids] (boardComments.tsx).
const reactionsShaped = (r) => r === undefined || (isObject(r) && Object.values(r).every((users) => Array.isArray(users) && users.every((u) => typeof u === 'string')))

// reactors is a message's reactions as "emoji\0user" strings.
function reactors(reactions) {
    if (!isObject(reactions)) return new Set()
    return new Set(Object.entries(reactions).flatMap(([emoji, users]) => (Array.isArray(users) ? users.map((u) => `${emoji}\0${u}`) : [])))
}

// The verdicts, as putBackSocketWrites asks: false (it stands), the field
// naming someone else (put back), or { stamp } (it stands as the stamp).
const verdict = ({ fault, value, stamped }) => fault ?? (stamped ? { stamp: value } : false)

// byPerson is the verdict on a map whose values each name a person by idF
// and nameF (noteAuthors; facilitation's rounds and timer).
export const byPerson = (idF, nameF) => (context, key, change, map) =>
    verdict(person(change.oldValue, map.get(key), writerOf(context), idF, nameF))

// aComment is the verdict on a pin and its thread: its maker, and each
// message's author, words and reactions.
export function aComment(context, key, change, map) {
    const after = map.get(key)
    if (!isObject(after)) return false
    const who = writerOf(context)
    const before = isObject(change.oldValue) ? change.oldValue : undefined
    const head = person(before, after, who, 'authorId', 'authorName')
    if (head.fault) return head.fault
    let stamped = head.stamped === true
    const was = new Map((Array.isArray(before?.messages) ? before.messages : []).filter(isObject).map((m) => [m.id, m]))
    if (!Array.isArray(after.messages)) return stamped ? { stamp: head.value } : false
    const messages = []
    for (const m of after.messages) {
        if (!isObject(m)) {
            messages.push(m)
            continue
        }
        const prior = was.get(m.id)
        const own = person(prior, m, who, 'authorId', 'authorName')
        if (own.fault) return `messages.${own.fault}`
        if (prior && own.value.authorId !== who.id && prior.body !== m.body) return 'messages.body'
        // Fails closed: reactions changed to any other shape are put back.
        if (!reactionsShaped(m.reactions) && JSON.stringify(m.reactions) !== JSON.stringify(prior?.reactions)) return 'messages.reactions'
        const then = reactors(prior?.reactions)
        const now = reactors(m.reactions)
        const changed = [...now].filter((r) => !then.has(r)).concat([...then].filter((r) => !now.has(r)))
        if (changed.some((r) => r.slice(r.indexOf('\0') + 1) !== who.id)) return 'messages.reactions'
        stamped ||= own.stamped === true
        messages.push(own.value)
    }
    return stamped ? { stamp: { ...head.value, messages } } : false
}

// heldBy is who a held note names ({ id, name }), or null for any other element.
function heldBy(el) {
    const data = isObject(el) && isObject(el.customData) ? el.customData : null
    return data && data.privateBy !== undefined ? { id: data.privateBy, name: data.privateName } : null
}

// aHeldNote is the verdict on an element: a note held for someone else, or
// a change to the hold on someone else's (taking it off, their name), is put
// back as a newer version every tab takes (a new one goes as deleted); one's
// own takes the writer's id and name, newer, and its author's reveal (the
// hold taken off) stands. Not person(): the reveal removes privateBy, which
// person() would stamp back. Any other change to an element stands.
export function aHeldNote(context, key, change, map) {
    const after = map.get(key)
    if (!isObject(after)) return false
    const who = writerOf(context)
    const before = isObject(change.oldValue) ? change.oldValue : undefined
    const was = heldBy(before)
    const now = heldBy(after)
    const field = 'customData.privateBy'
    const back = (why) => ({ stamp: before ? newer(before, after) : newer({ ...after, isDeleted: true }, after), told: why })
    if (now === null) return was && someoneElse(was.id, who) ? back(field) : false
    if (now.id !== was?.id) {
        if (someoneElse(now.id, who) || (was && someoneElse(was.id, who))) return back(field)
    } else if (someoneElse(now.id, who)) {
        return now.name === was.name ? false : back('customData.privateName')
    }
    if (now.id === who.id && (now.name === who.name || !who.name)) return false
    return { stamp: newer({ ...after, customData: { ...after.customData, privateBy: who.id, ...(who.name ? { privateName: who.name } : {}) } }, after) }
}

// aVote is the verdict on a vote: its key ends with its voter's id.
export function aVote(context, key, change, map) {
    return map.get(key) !== undefined && !String(key).endsWith(`:${writerOf(context).id}`) ? 'voter' : false
}

// toldOf logs a put-back on docName's map (the writer and the field only).
export const toldOf = (log, docName, mapName) => (context, key, why) => {
    if (typeof why !== 'string') return
    log.log(`[Collab] ${docName}: put back ${context.user?.id ?? 'a socket'}'s write naming someone else (${mapName}.${why})`)
}

// guardAuthors keeps a board's comments, note makers, votes and held notes
// written as their writers (facilitationGuard.js keeps its rounds, with
// byPerson).
export function guardAuthors(doc, docName, log) {
    putBackSocketWrites(doc, doc.getMap(ELEMENTS), AUTHOR_ORIGIN, aHeldNote, toldOf(log, docName, ELEMENTS))
    putBackSocketWrites(doc, doc.getMap(COMMENTS), AUTHOR_ORIGIN, aComment, toldOf(log, docName, COMMENTS))
    putBackSocketWrites(doc, doc.getMap(NOTE_AUTHORS), AUTHOR_ORIGIN, byPerson('id', 'name'), toldOf(log, docName, NOTE_AUTHORS))
    putBackSocketWrites(doc, doc.getMap(VOTES), AUTHOR_ORIGIN, aVote, toldOf(log, docName, VOTES))
}

// guardAgentEdits keeps a doc's record of where an agent wrote from every
// socket: only this service's ops write it (write.js).
export function guardAgentEdits(doc, docName, log) {
    putBackSocketWrites(doc, doc.getMap(AGENT_EDITS), AUTHOR_ORIGIN, () => 'agent', toldOf(log, docName, AGENT_EDITS))
}
