-- Durable, attempt-scoped billing commands. The JSONB command is immutable after enqueue;
-- billing effects and usage-log persistence are applied by the replay worker.
CREATE TABLE IF NOT EXISTS billing_attempt_outbox (
    id                   BIGSERIAL PRIMARY KEY,
    attempt_id           TEXT NOT NULL,
    api_key_id           BIGINT NOT NULL,
    request_fingerprint  TEXT NOT NULL,
    command              JSONB NOT NULL, -- command JSONB NOT NULL
    status               TEXT NOT NULL DEFAULT 'pending'
                         CHECK (status IN ('pending', 'processing', 'finalization_pending', 'finalizing', 'succeeded', 'terminal')),
    attempts             INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    apply_result         JSONB,
    available_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lease_until          TIMESTAMPTZ,
    leased_by            TEXT,
    last_error           TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT billing_attempt_outbox_identity UNIQUE (attempt_id, api_key_id)
);

-- Claims use SELECT ... FOR UPDATE SKIP LOCKED in the repository.
CREATE INDEX IF NOT EXISTS idx_billing_attempt_outbox_claim
    ON billing_attempt_outbox (status, available_at, id)
    WHERE status IN ('pending', 'processing');
CREATE INDEX IF NOT EXISTS idx_billing_attempt_outbox_finalize_claim
    ON billing_attempt_outbox (status, available_at, id)
    WHERE status IN ('finalization_pending', 'finalizing');
CREATE INDEX IF NOT EXISTS idx_billing_attempt_outbox_retention
    ON billing_attempt_outbox (updated_at, id);

COMMENT ON TABLE billing_attempt_outbox IS
    'Immutable attempt-scoped billing command envelope; terminal rows are retained for reconciliation and are never silently dropped.';
COMMENT ON COLUMN billing_attempt_outbox.command IS
    'Immutable JSONB billing command envelope; identity and fingerprint columns support conflict detection without parsing payloads.';
