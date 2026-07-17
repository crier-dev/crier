CREATE TABLE inbox_entries (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    payload JSONB NOT NULL,
    delivery_sequence BIGINT GENERATED ALWAYS AS IDENTITY NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    leased_at TIMESTAMPTZ NULL,
    lease_id TEXT NULL,
    lease_expires_at TIMESTAMPTZ NULL,
    acked BOOLEAN NOT NULL DEFAULT FALSE,

    CONSTRAINT inbox_entries_id_not_blank CHECK (length(btrim(id)) > 0),
    CONSTRAINT inbox_entries_expiry_after_creation CHECK (expires_at > created_at),
    CONSTRAINT inbox_entries_lease_fields_consistent CHECK (
        (leased_at IS NULL AND lease_id IS NULL AND lease_expires_at IS NULL)
        OR
        (leased_at IS NOT NULL AND lease_id IS NOT NULL AND lease_id <> ''
         AND lease_expires_at IS NOT NULL AND lease_expires_at > leased_at)
    ),
    CONSTRAINT inbox_entries_agent_id_fkey
        FOREIGN KEY (agent_id)
        REFERENCES agents(id)
        ON DELETE CASCADE
);

-- Required Retrieve predicate index: agent_id + acked + expires_at.
CREATE INDEX inbox_entries_retrieve_idx
    ON inbox_entries (agent_id, acked, expires_at)
    WHERE acked = FALSE;

-- Required FIFO/locking index: prevents sorting an entire agent queue for each retrieve.
CREATE INDEX inbox_entries_fifo_idx
    ON inbox_entries (agent_id, delivery_sequence)
    WHERE acked = FALSE;

-- Required Ack lookup index. The primary-key predicate remains authoritative.
CREATE INDEX inbox_entries_lease_id_idx
    ON inbox_entries (lease_id)
    WHERE lease_id IS NOT NULL;

-- Required TTL purge index.
CREATE INDEX inbox_entries_expires_at_idx
    ON inbox_entries (expires_at);

-- Required expired-lease release index.
CREATE INDEX inbox_entries_lease_expires_at_idx
    ON inbox_entries (lease_expires_at)
    WHERE lease_expires_at IS NOT NULL;
