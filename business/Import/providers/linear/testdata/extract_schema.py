"""Extract field types and deprecations for chosen GraphQL object types from
Linear's public schema.graphql (data only; nothing in it is executed).
usage: python3 -I linear_schema_extract.py <schema.graphql> <out.json> <commit> Type1 Type2 ..."""
import json, re, sys

src, out, commit = sys.argv[1], sys.argv[2], sys.argv[3]
roots = sys.argv[4:]
text = open(src, encoding="utf-8").read()
# Drop block descriptions and one-line descriptions.
text = re.sub(r'"""[\s\S]*?"""', '', text)
text = re.sub(r'^\s*"[^"\n]*"\s*$', '', text, flags=re.M)

types = {}
for m in re.finditer(r'^(type|interface)\s+(\w+)[^{]*\{([\s\S]*?)^\}', text, flags=re.M):
    name, body = m.group(2), m.group(3)
    fields = {}
    i, n = 0, len(body)
    # Fields: name, optional (args...), ':' type, optional directives.
    for fm in re.finditer(r'^\s{2}(\w+)\s*(\((?:[^()]|\([^()]*\))*\))?\s*:\s*([\[\]\w!]+)([^\n]*)', body, flags=re.M):
        fname, ftype, rest = fm.group(1), fm.group(3), fm.group(4)
        base = re.sub(r'[\[\]!]', '', ftype)
        fields[fname] = {"type": base, "deprecated": "@deprecated" in rest}
    types[name] = fields

keep, todo = {}, list(roots)
while todo:
    t = todo.pop()
    if t in keep or t not in types:
        continue
    keep[t] = types[t]
json.dump({"source": "github.com/linear/linear packages/sdk/src/schema.graphql @ " + commit,
           "note": "Only the types the import's queries reach; regenerate with proj-4 tools/linear_schema_extract.py.",
           "types": keep}, open(out, "w"), indent=1, sort_keys=True)
print(len(types), "types parsed;", len(keep), "kept")
