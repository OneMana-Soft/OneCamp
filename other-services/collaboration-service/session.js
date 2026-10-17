// The Doc Session's side of the server (docs programme A2): who may do what with
// a doc they open live, and the messages that tell their tab.
//
// WHY. A doc used to connect only for someone who could edit: the API answered
// /docColab/authorize with 403 to everyone else, so a member who could read or
// comment got the saved body, up to 30 s old, that never moved. Now everyone
// who may read joins live. The API says how far (/docColab/session: level,
// canEdit, locked), a reader's connection is read-only (Hocuspocus drops
// anything it sends), and the tab is told the decision in a "session" message,
// with the doc's lineage, so it can check it holds the same copy before it
// syncs (web hooks/useDocSession.ts).
//
// Messages (stateless, JSON):
//   {type:'session', level, canEdit, locked, lineage, schemaVersion}
//       once per connection, right after it is set up;
//   {type:'persisted', at, sv, lineage}
//       after each successful store of the doc, and as a tab connects: A4's
//       save path sends it (persisted.js persistedMessage; `at` in ms), so the
//       save dot says "saved" only once stored (I8).

// The route the API answers the session on. A service talking to an API from
// before it falls back to /docColab/authorize (see authorizeDoc).
export const SESSION_PATH = '/docColab/session'
export const LEGACY_AUTHORIZE_PATH = '/docColab/authorize'

// Why a doc's connection was refused, as the tab reads it (permission-denied
// reason). A refusal carries one of these instead of "permission-denied".
export const REFUSED = {
    // The token is not (or no longer) good: the tab fetches a fresh one.
    unauthenticated: 'unauthenticated',
    // This person may not read the doc.
    forbidden: 'forbidden',
    // No such doc, or not one this person was ever shown.
    notFound: 'not-found',
    // In the trash: nobody joins it live.
    gone: 'gone',
    // The API didn't answer: try again later.
    unavailable: 'auth-unavailable',
}

const LEVELS = new Set(['none', 'view', 'comment', 'edit', 'full'])

// accessFrom is the API's answer as the session holds it.
export function accessFrom(data) {
    const level = LEVELS.has(data?.level) ? data.level : 'view'
    return { level, canEdit: data?.canEdit === true, locked: data?.locked === true }
}

const refused = (reason, message) => Object.assign(new Error(message || `refused: ${reason}`), { reason })

// reasonFor is the refusal a failed API answer means.
export function reasonFor(status) {
    if (status === 401) return REFUSED.unauthenticated
    if (status === 403) return REFUSED.forbidden
    if (status === 404) return REFUSED.notFound
    if (status === 410) return REFUSED.gone
    return REFUSED.unavailable
}

// A 404 that is chi's "no such route" rather than the API's "no such doc"
// (which is JSON with a msg): an API from before /docColab/session.
const routeMissing = (e) => e?.response?.status === 404 && typeof e.response.data?.msg !== 'string'

// authorizeDoc asks the API how far the person holding `token` may go with doc
// `id`: {level, canEdit, locked}. It throws an Error with a `reason` from
// REFUSED when they may not join.
export async function authorizeDoc({ api, backendUrl, token, id, secret }) {
    const body = { doc_uuid: id }
    const bearer = { Authorization: `Bearer ${token}` }
    try {
        const r = await api.post(`${backendUrl}${SESSION_PATH}`, body, { headers: { ...bearer, 'X-Internal-Secret': secret } })
        return accessFrom(r?.data)
    } catch (e) {
        if (!routeMissing(e)) throw refused(reasonFor(e?.response?.status), e?.message)
    }
    // An API from before the session: 200 only to someone who may edit.
    try {
        await api.post(`${backendUrl}${LEGACY_AUTHORIZE_PATH}`, body, { headers: bearer })
        return { level: 'edit', canEdit: true, locked: false }
    } catch (e) {
        throw refused(reasonFor(e?.response?.status), e?.message)
    }
}

// sessionMessage is what a tab is told once its connection is set up.
export function sessionMessage({ access, lineage = null, schemaVersion = null, readOnly = false }) {
    return JSON.stringify({
        type: 'session',
        level: access.level,
        canEdit: access.canEdit && !readOnly,
        locked: access.locked,
        lineage: lineage ?? null,
        schemaVersion: schemaVersion ?? null,
    })
}
