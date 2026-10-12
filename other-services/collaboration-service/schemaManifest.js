// The shape of an editor schema that both sides must agree on: every node and
// mark by name, with its attribute names and where it may sit. The web repo
// builds the same shape from its editor (components/minimal-tiptap/
// schemaManifest.test.ts, same function), so the two compare as JSON.

const sorted = (o) => Object.fromEntries(Object.keys(o).sort().map((k) => [k, o[k]]))

export function manifestOf(schema) {
    const nodes = {}
    for (const [name, type] of Object.entries(schema.nodes)) {
        nodes[name] = {
            attrs: Object.keys(type.spec.attrs || {}).sort(),
            group: type.spec.group ?? null,
            content: type.spec.content ?? null,
            inline: type.isInline,
            atom: type.isAtom,
        }
    }
    const marks = {}
    for (const [name, type] of Object.entries(schema.marks)) {
        marks[name] = { attrs: Object.keys(type.spec.attrs || {}).sort() }
    }
    return { nodes: sorted(nodes), marks: sorted(marks) }
}
