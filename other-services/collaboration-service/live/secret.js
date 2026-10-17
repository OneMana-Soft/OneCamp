// The internal secret: the one credential the Live Doc Writer's port accepts.

import { timingSafeEqual } from 'node:crypto'

// internalSecretOK compares the presented secret with the configured one in
// constant time. An unset or blank configured secret never matches: a default
// written in code would be public, and an install left on it would let anyone
// who read the code write into every document.
export function internalSecretOK(presented, configured) {
    if (typeof configured !== 'string' || configured.trim() === '') return false
    if (typeof presented !== 'string') return false
    const a = Buffer.from(presented)
    const b = Buffer.from(configured)
    return a.length === b.length && timingSafeEqual(a, b)
}
