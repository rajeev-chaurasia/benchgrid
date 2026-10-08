-- Applied on every start. Every statement is idempotent so that any number of
-- server replicas can start at once against one database.

CREATE TABLE IF NOT EXISTS rigs (
    id               text PRIMARY KEY,
    descriptor       jsonb NOT NULL,
    -- What the agent last reported, READY or QUARANTINED. Whether the rig is
    -- leased is not a state here, it is the lease columns below.
    agent_state      text NOT NULL DEFAULT 'READY',
    agent_reason     text NOT NULL DEFAULT '',
    last_heartbeat   timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- The fence only ever increases. It is the rig's own counter and means
    -- nothing on any other rig.
    fence            bigint NOT NULL DEFAULT 0,
    holder           text,
    experiment_id    text,
    attempt          integer,
    expires_at       timestamptz,
    last_released_at timestamptz
);

CREATE TABLE IF NOT EXISTS experiments (
    id              text PRIMARY KEY,
    idempotency_key text UNIQUE,
    spec            jsonb NOT NULL,
    spec_sha256     text NOT NULL,
    state           text NOT NULL DEFAULT 'QUEUED',
    status_reason   text NOT NULL DEFAULT '',
    attempt         integer NOT NULL DEFAULT 0,
    max_attempts    integer NOT NULL DEFAULT 3,
    rig_id          text,
    fence           bigint,
    submitted_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at     timestamptz
);

CREATE INDEX IF NOT EXISTS experiments_queue ON experiments (submitted_at) WHERE state = 'QUEUED';
CREATE INDEX IF NOT EXISTS experiments_running ON experiments (rig_id) WHERE state = 'RUNNING';

CREATE TABLE IF NOT EXISTS attempts (
    experiment_id text NOT NULL REFERENCES experiments (id),
    attempt       integer NOT NULL,
    rig_id        text NOT NULL,
    fence         bigint NOT NULL,
    scheduler     text NOT NULL,
    leased_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    dispatched_at timestamptz,
    finished_at   timestamptz,
    status        text NOT NULL DEFAULT 'LEASED',
    status_reason text NOT NULL DEFAULT '',
    PRIMARY KEY (experiment_id, attempt)
);

-- A rig's latest calibration canary. Added after the first schema, so it is
-- an ALTER that every replica can run on start without coordinating.
ALTER TABLE rigs ADD COLUMN IF NOT EXISTS canary_cv double precision;
ALTER TABLE rigs ADD COLUMN IF NOT EXISTS canary_median_ns double precision;
ALTER TABLE rigs ADD COLUMN IF NOT EXISTS canary_at timestamptz;
