CREATE TABLE agents (
    id TEXT PRIMARY KEY,
    public_key BYTEA NOT NULL,
    capabilities JSONB NOT NULL DEFAULT '[]'::jsonb,
    status TEXT NOT NULL DEFAULT 'online',
    registered_at TIMESTAMPTZ NOT NULL,
    last_seen TIMESTAMPTZ NOT NULL,

    CONSTRAINT agents_id_not_blank CHECK (length(btrim(id)) > 0),
    CONSTRAINT agents_public_key_ed25519_length CHECK (octet_length(public_key) = 32),
    CONSTRAINT agents_capabilities_is_array CHECK (jsonb_typeof(capabilities) = 'array'),
    CONSTRAINT agents_status_valid CHECK (status IN ('online', 'offline'))
);

CREATE INDEX agents_status_last_seen_idx
    ON agents (status, last_seen DESC);
