import { test } from 'node:test'
import assert from 'node:assert/strict'
import { dateWords } from './connected.js'

test('a date on no calendar has no words, so no reader is shown it', () => {
    assert.equal(dateWords('2026-02-30'), '')
    assert.equal(dateWords('2026-13-01'), '')
    assert.equal(dateWords('2026-10-01'), '1 Oct 2026')
    assert.equal(dateWords('2028-02-29'), '29 Feb 2028')
})
