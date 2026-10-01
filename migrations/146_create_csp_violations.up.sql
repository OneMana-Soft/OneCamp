-- What the Content-Security-Policy would have blocked, aggregated.
--
-- The collector added earlier logs each report and nothing else, so the evidence
-- lives in container logs and is wiped by every redeploy. A validation window
-- measured in weeks cannot survive there, which means the policy could never be
-- promoted from Report-Only on that evidence.
--
-- AGGREGATED, NOT APPENDED. One busy page can emit a report per load, and a
-- single browser extension can produce thousands that have nothing to do with
-- the site. Storing every report would make this the largest table here within
-- days and would answer a question nobody asked. The question that matters is
-- "which distinct things would break if I enforced this", which is a set, not a
-- stream.
--
-- KEYED AT ORIGIN GRANULARITY, because that is the granularity a CSP allowlist
-- is written in: you permit https://example.com, not one path on it. Collapsing
-- to the origin also absorbs cache-busting paths — the first real violation seen
-- here was Cloudflare's own analytics beacon, whose URL carries a build hash
-- that would otherwise create a new row on every one of their deploys.
CREATE TABLE IF NOT EXISTS csp_violations (
    id           uuid PRIMARY KEY,

    -- The identity of a violation: which rule, what it blocked, where.
    directive    text NOT NULL,
    blocked_origin text NOT NULL,
    document_path  text NOT NULL,

    -- report | enforce. Report-Only and enforcing violations are different
    -- facts: one is a warning about the future, the other is a live breakage.
    disposition  text NOT NULL DEFAULT 'report',

    times_seen   bigint      NOT NULL DEFAULT 1,
    first_seen   timestamptz NOT NULL DEFAULT now(),
    last_seen    timestamptz NOT NULL DEFAULT now()
);

-- The upsert key. Without this the aggregation is not an aggregation.
CREATE UNIQUE INDEX IF NOT EXISTS idx_csp_violations_identity
    ON csp_violations (directive, blocked_origin, document_path, disposition);

-- The read an operator actually performs: what is still happening, worst first.
CREATE INDEX IF NOT EXISTS idx_csp_violations_recent
    ON csp_violations (last_seen DESC);
