-- Call Quality Telemetry (calls A5, #56): what each person's own app saw of
-- their connection in a call, every 10 s. Numbers only: no names and no
-- addresses, just the session and the call's identity. Insert-only; a sample
-- is kept 30 days once its day is rolled up into call_quality_daily, then
-- purged (business/CallQuality). No foreign key to call_sessions: the call
-- state migration's down step must still drop its table.
CREATE TABLE IF NOT EXISTS call_quality_samples (
    "id"          bigserial PRIMARY KEY,
    "session_id"  uuid NOT NULL,
    "identity"    varchar(96) NOT NULL,
    -- The start of the sample's 10 s.
    "at"          timestamptz NOT NULL,
    "rtt_ms"      integer NOT NULL DEFAULT 0,
    "jitter_ms"   integer NOT NULL DEFAULT 0,
    "loss_pct"    real NOT NULL DEFAULT 0,
    "send_kbps"   integer NOT NULL DEFAULT 0,
    "recv_kbps"   integer NOT NULL DEFAULT 0,
    "fps"         real NOT NULL DEFAULT 0,
    "freeze_ms"   integer NOT NULL DEFAULT 0,
    "limited"     varchar(16) NOT NULL DEFAULT '' CHECK (limited IN ('', 'cpu', 'bandwidth', 'other')),
    "cpu"         varchar(16) NOT NULL DEFAULT '' CHECK (cpu IN ('', 'nominal', 'fair', 'serious', 'critical'))
);
-- One sample per person per 10 s of a call: a batch sent again (its answer
-- lost) adds nothing, and no app can send more than that. It also serves a
-- call's reads, by session.
CREATE UNIQUE INDEX IF NOT EXISTS call_quality_samples_one ON call_quality_samples (session_id, identity, at);
CREATE INDEX IF NOT EXISTS call_quality_samples_at ON call_quality_samples (at);

-- One row a day for the whole install: how its calls went (zoom-17).
CREATE TABLE IF NOT EXISTS call_quality_daily (
    "day"            date PRIMARY KEY,
    "calls"          integer NOT NULL,
    "people"         integer NOT NULL,
    "samples"        integer NOT NULL,
    "rtt_p50_ms"     integer NOT NULL,
    "rtt_p95_ms"     integer NOT NULL,
    "loss_p95_pct"   real NOT NULL,
    "freeze_share"   real NOT NULL,
    "cpu_share"      real NOT NULL,
    "network_share"  real NOT NULL,
    "rolled_at"      timestamptz NOT NULL DEFAULT now()
);
