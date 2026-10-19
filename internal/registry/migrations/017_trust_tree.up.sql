CREATE TABLE trust_anchors (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    public_key TEXT NOT NULL UNIQUE CHECK (public_key ~ '^[0-9a-f]{64}$'),
    label TEXT NOT NULL CHECK (btrim(label) <> ''),
    created_at TIMESTAMPTZ NOT NULL
);

-- Keep every signed field exactly; timestamptz would round issued_at and
-- invalidate signatures after reload. subject_key is an indexed projection.
CREATE TABLE trust_delegations (
    signature TEXT PRIMARY KEY CHECK (signature ~ '^[0-9a-f]{128}$'),
    subject_key TEXT NOT NULL CHECK (subject_key ~ '^[0-9a-f]{64}$'),
    record JSONB NOT NULL CHECK (jsonb_typeof(record) = 'object')
);
CREATE INDEX trust_delegations_subject_idx ON trust_delegations(subject_key);
