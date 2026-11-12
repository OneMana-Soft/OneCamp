-- Undoes 583 without losing a formula: the type check is as 192 made it and
-- the formula-only check goes, but the formula column stays with every
-- formula in it, and the live formula fields go to the trash (soft: their
-- deletion time is set). The code before J-F reads live fields only and knows
-- no formula type, so it never meets one; 583 up again finds each formula
-- where it was, and a project admin brings a field back from the trash. A
-- field already in the trash keeps its own deletion time. The old type check
-- is added NOT VALID, as the trashed formula fields keep their type: it holds
-- for every field made or changed from here on.
UPDATE task_fields SET deleted_at = NOW() WHERE type = 'formula' AND deleted_at IS NULL;
ALTER TABLE task_fields DROP CONSTRAINT IF EXISTS task_fields_formula_only_check;
ALTER TABLE task_fields DROP CONSTRAINT IF EXISTS task_fields_type_check;
ALTER TABLE task_fields ADD CONSTRAINT task_fields_type_check
    CHECK (type IN ('text', 'number', 'money', 'date', 'select', 'multi_select', 'person', 'checkbox', 'url')) NOT VALID;
