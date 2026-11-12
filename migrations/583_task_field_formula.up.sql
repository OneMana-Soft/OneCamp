-- J-F (jira-01): a project's task field may be a formula, worked out from the
-- task's other fields and its own (estimate, points, dates, status, priority,
-- when it was made) on each read, by the Formula Engine tables use. Its
-- values are never stored: the field keeps only its formula, with the fields
-- it reads named by id ({#<id>}), so renaming one doesn't break it.
ALTER TABLE task_fields DROP CONSTRAINT IF EXISTS task_fields_type_check;
ALTER TABLE task_fields ADD CONSTRAINT task_fields_type_check
    CHECK (type IN ('text', 'number', 'money', 'date', 'select', 'multi_select', 'person', 'checkbox', 'url', 'formula'));
-- A formula is written in at most 2000 characters (business/Formula
-- MaxLength); stored, each field it names is {#<id>}, so the shortest name
-- ({x}, 3 characters) becomes 39: at most 13 times as long.
-- 583's down keeps the column with its formulas, so up again finds it there:
-- the column and its checks are added apart, each check dropped first.
ALTER TABLE task_fields ADD COLUMN IF NOT EXISTS "formula" text;
ALTER TABLE task_fields DROP CONSTRAINT IF EXISTS task_fields_formula_length_check;
ALTER TABLE task_fields ADD CONSTRAINT task_fields_formula_length_check
    CHECK (char_length(formula) <= 26000);
-- A formula field has a formula; no other field has one.
ALTER TABLE task_fields DROP CONSTRAINT IF EXISTS task_fields_formula_only_check;
ALTER TABLE task_fields ADD CONSTRAINT task_fields_formula_only_check
    CHECK ((type = 'formula') = (formula IS NOT NULL));
