# Writes testdata/rowquery/meanings.json (tables T5c): one table's fields
# and rows in the API's shapes, cases of a view's rules as the browser keeps
# them, the spec each maps to (the web's lib/tables/query.ts rulesToSpec),
# and the rows each shows in order, which the server's evaluator decides.
#
# To change it: edit the cases here, then, from the backend's root,
#   python3 testdata/rowquery/meanings.py > testdata/rowquery/meanings.json
#   WRITE_MEANINGS=/tmp/ids.json go test -run '^TestTheBrowsersRulesMeanWhatTheServersDo$' ./business/RowQuery/
#   python3 testdata/rowquery/meanings.py --ids /tmp/ids.json > testdata/rowquery/meanings.json
# and copy the JSON, byte for byte, to the web's testdata/rowquery/. Check
# the new ids by hand before trusting them: they are the server's meaning,
# which the web's meanings.test.ts and the SQL (meanings_integration_test.go)
# are then held to.
import json, sys

T = "7ab1e000-0000-4000-8000-000000000001"
def fid(n): return "f1e1d000-0000-4000-8000-%012d" % n
def rid(n): return "40000000-0000-4000-8000-%012d" % n
ITEM, STAGE, DUE, COST, PAID, TAGS, MADE, NO, FILES, OWNERS = (fid(i) for i in range(1, 11))
# Formulas and a rollup (airtable-03), and the link and field the rollup adds up.
DOUBLE, LATE, CODE, OWED, VENDOR, AMOUNT = (fid(i) for i in range(11, 17))
# Who asks (@me), and someone else; a read sends a person as an id and a name.
ME, PRIYA = "0e000000-0000-4000-8000-000000000001", "0e000000-0000-4000-8000-000000000002"
def person(i, name): return {"id": i, "name": name}

def field(i, name, typ, config=None, pos=0):
    return {"id": i, "table_id": T, "name": name, "type": typ, "config": json.dumps(config or {}, ensure_ascii=True, separators=(",", ":")), "position": pos}

fields = [
    field(ITEM, "Item", "text", pos=0),
    field(STAGE, "Stage", "select", {"options": [{"id": "o1", "label": "Todo"}, {"id": "o2", "label": "Doing"}, {"id": "o3", "label": "Done"}, {"id": "o4", "label": "İstanbul"}]}, 1),
    field(DUE, "Due", "date", pos=2),
    field(COST, "Cost", "number", pos=3),
    field(PAID, "Paid", "checkbox", pos=4),
    field(TAGS, "Tags", "multi_select", {"options": [{"id": "v", "label": "Video"}, {"id": "p", "label": "Print"}, {"id": "e", "label": "Events"}]}, 5),
    field(MADE, "Made", "created_time", pos=6),
    field(NO, "No", "autonumber", pos=7),
    field(FILES, "Files", "attachment", pos=8),
    field(OWNERS, "Owners", "people", pos=9),
    # As a read sends them: what each gives in its config's result, and its
    # value worked out in the row's values (worked, below).
    field(DOUBLE, "Double", "formula", {"formula": "{Cost} * 2", "result": "number"}, 10),
    field(LATE, "Late", "formula", {"formula": "NOT({Paid})", "result": "checkbox"}, 11),
    field(CODE, "Code", "formula", {"formula": "UPPER({Item})", "result": "text"}, 12),
    field(OWED, "Owed", "rollup", {"relation": VENDOR, "field": AMOUNT, "aggregate": "sum", "result": "number"}, 13),
]

# (values, created_at); a row's number and its place follow its order.
cells = [
    ({ITEM: "Row 10", STAGE: "Done", DUE: "2026-10-18", COST: 1200, PAID: True, TAGS: ["Video"], FILES: [{"name": "brief.pdf"}], OWNERS: [person(ME, "Sam")]}, "2026-10-18T09:00:00Z"),
    ({ITEM: "row 2", STAGE: "todo ", DUE: "2026-10-18T20:00:00Z", COST: 900, PAID: False, TAGS: [" Video ", "Print"], OWNERS: [person(PRIYA, "Priya"), person(ME.upper(), "Sam")]}, "2026-10-18T10:00:00.25Z"),
    ({ITEM: " \t ", STAGE: "Archived", DUE: "2026-10-18T03:00:00Z", COST: "1,200", TAGS: ["  "]}, "2026-10-18T10:00:00.1Z"),
    ({ITEM: "Éclair", STAGE: "Doing", DUE: "2026-02-30", COST: 40.5, OWNERS: [person(PRIYA, "Priya")]}, "2026-10-18T20:00:00Z"),
    ({ITEM: "eclair 3", STAGE: "", DUE: "2026-10-17T23:00:00-05:00", COST: -3, PAID: True, TAGS: ["Events"], OWNERS: []}, "2026-10-18T18:29:59.999999Z"),
    ({ITEM: "", STAGE: 5, DUE: "soon", COST: None}, "2026-10-18T18:30:00Z"),
    ({ITEM: " ", STAGE: "istanbul", DUE: "2026-10-19", COST: 1e21}, "2026-10-17T12:00:00+05:30"),
    ({ITEM: "﻿", STAGE: "Zebra", DUE: "2026-10-18T14:30:00.5Z"}, "2026-10-19T00:00:00Z"),
    ({ITEM: "İSTANBUL", DUE: "2026-10-18T20:00", COST: 0}, "2026-10-18T09:00:00.000001Z"),
    ({ITEM: "ΟΔΟΣ", STAGE: "DONE"}, "2026-10-18T09:00:00Z"),
    ({ITEM: "  Paid ", STAGE: "Todo"}, "2026-10-18T09:00:01Z"),
    ({ITEM: 42, STAGE: "Doing"}, "2026-10-18T09:00:02Z"),
    ({ITEM: True}, "2026-10-18T09:00:03Z"),
    ({ITEM: "Booth", FILES: [{"name": "  "}]}, "2026-10-18T09:00:04Z"),
    ({ITEM: "BOOTH 007", PAID: True}, "2026-10-18T09:00:05Z"),
    ({ITEM: "booth 7", TAGS: ["video"], OWNERS: [person(ME, "")]}, "2026-10-18T09:00:06Z"),
    ({ITEM: "\u0085"}, "2026-10-18T09:00:07Z"),
]
from datetime import datetime, timezone
def stamp(t):
    # A row's stamp as a read writes it into its values (business/FieldTypes stamp): RFC 3339, the second, in UTC.
    return datetime.fromisoformat(t.replace("Z", "+00:00")).astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
# What a read works out for each row's formulas and rollup, by row number:
# a number, a tick, a text, or nothing (it can't be worked out, or there
# is nothing to add up). The browser reads them from the row's values, and
# the evaluator compares them as what each field gives.
worked = {
    1: {DOUBLE: 2400, LATE: False, CODE: "ROW 10", OWED: 300},
    2: {DOUBLE: 1800, LATE: True, CODE: "ROW 2", OWED: 0},
    3: {LATE: True, CODE: ""},
    4: {DOUBLE: 81, LATE: True, CODE: "ÉCLAIR", OWED: 1200.5},
    5: {DOUBLE: -6, LATE: False, CODE: "ECLAIR 3", OWED: -10},
    6: {LATE: True},
    7: {DOUBLE: 2e21, CODE: " "},
    9: {DOUBLE: 0, LATE: True, CODE: "İSTANBUL", OWED: 0.5},
    10: {CODE: "ΟΔΟΣ", OWED: 300},
}
rows = []
for n, (vals, made) in enumerate(cells, 1):
    # As the API sends a row: its number and its stamps in its values too.
    vals = dict(vals, **{NO: n, MADE: stamp(made)}, **worked.get(n, {}))
    rows.append({"id": rid(n), "table_id": T, "values": json.dumps({k: v for k, v in vals.items()}, ensure_ascii=True, separators=(",", ":")),
                 "position": n, "position_key": "a%02d" % n, "row_no": n, "created_at": made, "updated_at": made})

ref = lambda f: "field:" + f
def sort(f, d="asc"): return {"field": f, "dir": d}
def flt(f, op, v=None):
    r = {"field": f, "op": op}
    if v is not None: r["value"] = v
    return r
def cond(f, op, values=None):
    c = {"field": ref(f), "operator": op}
    if values: c["values"] = values
    return c
txt = lambda s: {"text": s}
num = lambda n: {"number": n}
def rules(sorts=(), filters=(), match="all"): return {"sort": list(sorts), "filters": list(filters), "match": match}
# A group of filters inside a view's, as the browser keeps it, and as the spec has it.
def grp(match, *filters): return {"match": match, "filters": list(filters)}
def sgrp(op, *filters): return {"op": op, "filters": list(filters)}
def spec(sorts=(), filters=None, op="and"):
    s = {}
    if filters is not None: s["filter"] = {"op": op, "filters": filters}
    if sorts: s["sorts"] = [({"field": ref(f), "desc": True} if d == "desc" else {"field": ref(f)}) for f, d in sorts]
    return s

cases = []
def case(name, r, s):
    cases.append({"name": name, "rules": r, "spec": s, "ids": []})

for f, name in [(ITEM, "text"), (STAGE, "a choice"), (DUE, "a date"), (COST, "a number"), (PAID, "a tick"), (MADE, "a row's creation"), (NO, "a row's number")]:
    for d in ("asc", "desc"):
        case(f"{name}, sorted {d}", rules([sort(f, d)]), spec([(f, d)]))
case("a choice, then text the other way", rules([sort(STAGE), sort(ITEM, "desc")]), spec([(STAGE, "asc"), (ITEM, "desc")]))

for op, v in [("is", "paid"), ("is_not", "paid"), ("contains", "istanbul"), ("not_contains", "booth"), ("is", "οδοσ"), ("contains", "42"), ("is", "true")]:
    case(f"text {op} {v!r}", rules(filters=[flt(ITEM, op, v)]), spec(filters=[cond(ITEM, op, [txt(v)])]))
for op in ("empty", "not_empty"):
    case(f"text {op}", rules(filters=[flt(ITEM, op)]), spec(filters=[cond(ITEM, op)]))
for op, v in [("is", "done"), ("is_not", "done"), ("contains", "o")]:
    case(f"a choice {op} {v!r}", rules(filters=[flt(STAGE, op, v)]), spec(filters=[cond(STAGE, op, [txt(v)])]))
case("a choice doesn't contain 'o'", rules(filters=[flt(STAGE, "not_contains", "o")]), spec(filters=[cond(STAGE, "not_contains", [txt("o")])]))
case("a choice empty", rules(filters=[flt(STAGE, "empty")]), spec(filters=[cond(STAGE, "empty")]))
for op, v in [("is", "2026-10-19"), ("is", "2026-10-18"), ("before", "2026-10-19"), ("after", "2026-10-18")]:
    case(f"a date {op} {v}", rules(filters=[flt(DUE, op, v)]), spec(filters=[cond(DUE, op, [txt(v)])]))
case("a date empty", rules(filters=[flt(DUE, "empty")]), spec(filters=[cond(DUE, "empty")]))
case("a date that isn't one hides nothing", rules(filters=[flt(DUE, "is", "2026-02-30")]), spec())
case("a row's creation on a day", rules(filters=[flt(MADE, "is", "2026-10-19")]), spec(filters=[cond(MADE, "is", [txt("2026-10-19")])]))
for op, v, n in [("gt", "1000", 1000), ("lte", "40.5", 40.5), ("is_not", "900", 900), ("is", "0", 0)]:
    case(f"a number {op} {v}", rules(filters=[flt(COST, op, v)]), spec(filters=[cond(COST, op, [num(n)])]))
case("a number empty", rules(filters=[flt(COST, "empty")]), spec(filters=[cond(COST, "empty")]))
case("a number that isn't one hides nothing", rules(filters=[flt(COST, "gt", "lots")]), spec())
case("a row's number at least 10", rules(filters=[flt(NO, "gte", "10")]), spec(filters=[cond(NO, "gte", [num(10)])]))
for op in ("checked", "unchecked"):
    case(f"a tick {op}", rules(filters=[flt(PAID, op)]), spec(filters=[cond(PAID, op)]))
case("a list has 'video'", rules(filters=[flt(TAGS, "contains", "video")]), spec(filters=[cond(TAGS, "is", [txt("video")])]))
case("a list doesn't have 'video'", rules(filters=[flt(TAGS, "not_contains", "video")]), spec(filters=[cond(TAGS, "is_not", [txt("video")])]))
case("a list empty", rules(filters=[flt(TAGS, "empty")]), spec(filters=[cond(TAGS, "empty")]))
case("files have 'brief.pdf'", rules(filters=[flt(FILES, "contains", "brief.pdf")]), spec(filters=[cond(FILES, "is", [txt("brief.pdf")])]))
case("files empty", rules(filters=[flt(FILES, "empty")]), spec(filters=[cond(FILES, "empty")]))
case("any of two", rules(filters=[flt(ITEM, "contains", "booth"), flt(COST, "gt", "1000")], match="any"),
     spec(filters=[cond(ITEM, "contains", [txt("booth")]), cond(COST, "gt", [num(1000)])], op="or"))
case("any of two, one not filled in, hides nothing", rules(filters=[flt(ITEM, "contains", "booth"), flt(COST, "gt", "")], match="any"), spec())
case("all of two, one not filled in, is the other", rules(filters=[flt(ITEM, "contains", "row"), flt(COST, "gt", " ")]), spec(filters=[cond(ITEM, "contains", [txt("row")])]))
case("people have me", rules(filters=[flt(OWNERS, "contains", "@me")]), spec(filters=[cond(OWNERS, "is", [txt("@me")])]))
case("people don't have me", rules(filters=[flt(OWNERS, "not_contains", "@me")]), spec(filters=[cond(OWNERS, "is_not", [txt("@me")])]))
case("groups three deep", rules(filters=[flt(ITEM, "not_empty"), grp("any", flt(PAID, "checked"), grp("all", flt(STAGE, "not_contains", "o"), flt(COST, "gt", "100")))]),
     spec(filters=[cond(ITEM, "not_empty"), sgrp("or", cond(PAID, "checked"), sgrp("and", cond(STAGE, "not_contains", [txt("o")]), cond(COST, "gt", [num(100)])))]))
case("any of me or a group", rules(filters=[flt(OWNERS, "contains", "@me"), grp("all", flt(TAGS, "contains", "events"), flt(COST, "lte", "1000"))], match="any"),
     spec(filters=[cond(OWNERS, "is", [txt("@me")]), sgrp("and", cond(TAGS, "is", [txt("events")]), cond(COST, "lte", [num(1000)]))], op="or"))
case("a group of any, one not filled in, hides nothing", rules(filters=[flt(ITEM, "contains", "row"), grp("any", flt(COST, "gt", ""), flt(PAID, "checked"))]),
     spec(filters=[cond(ITEM, "contains", [txt("row")])]))
case("a group of all, one not filled in, is the other", rules(filters=[grp("all", flt(COST, "gt", "x"), flt(PAID, "unchecked"))], match="any"),
     spec(filters=[cond(PAID, "unchecked")], op="or"))
case("an empty group hides nothing", rules(filters=[grp("all"), flt(PAID, "checked")]), spec(filters=[cond(PAID, "checked")]))
case("a filter and two sorts", rules([sort(PAID, "desc"), sort(ITEM)], [flt(ITEM, "not_empty")]),
     spec([(PAID, "desc"), (ITEM, "asc")], filters=[cond(ITEM, "not_empty")]))

# A formula's and a rollup's values, compared as what each gives (airtable-03).
for f, name in [(DOUBLE, "a formula's number"), (LATE, "a formula's tick"), (CODE, "a formula's text"), (OWED, "a rollup's number")]:
    for d in ("asc", "desc"):
        case(f"{name}, sorted {d}", rules([sort(f, d)]), spec([(f, d)]))
case("a formula's number over 100", rules(filters=[flt(DOUBLE, "gt", "100")]), spec(filters=[cond(DOUBLE, "gt", [num(100)])]))
case("a formula's number empty", rules(filters=[flt(DOUBLE, "empty")]), spec(filters=[cond(DOUBLE, "empty")]))
case("a formula's tick checked", rules(filters=[flt(LATE, "checked")]), spec(filters=[cond(LATE, "checked")]))
case("a formula's text contains 'row'", rules(filters=[flt(CODE, "contains", "row")]), spec(filters=[cond(CODE, "contains", [txt("row")])]))
case("a rollup's number at least 0", rules(filters=[flt(OWED, "gte", "0")]), spec(filters=[cond(OWED, "gte", [num(0)])]))

out = {
    "about": "One table's fields and rows in the API's shapes, a view's rules as the browser keeps them, the spec each maps to (lib/tables/query.ts rulesToSpec), and the rows each shows, in order, as the server's evaluator (business/RowQuery Oracle) answers: the browser's applyViewRules must show the same. The same bytes in both repos.",
    "now": "2026-10-21T06:30:00Z",
    "zone": "Asia/Kolkata",
    "me": ME,
    "table": T,
    "fields": fields,
    "rows": rows,
    "cases": cases,
}
if len(sys.argv) > 1 and sys.argv[1] == "--ids":
    old = json.load(open(sys.argv[2]))
    byname = {c["name"]: c["ids"] for c in old["cases"]}
    for c in cases: c["ids"] = byname.get(c["name"], [])
json.dump(out, sys.stdout, ensure_ascii=True, indent=1)
print()
