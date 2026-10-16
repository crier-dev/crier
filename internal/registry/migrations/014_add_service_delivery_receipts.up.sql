CREATE TABLE service_delivery_receipts (
    sender TEXT NOT NULL,
    recipient TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    body_digest TEXT NOT NULL,
    status_code INTEGER NOT NULL,
    message_id TEXT NOT NULL UNIQUE,
    response_body BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (sender, recipient, idempotency_key),
    CONSTRAINT service_delivery_sender_not_blank CHECK (length(sender) > 0),
    CONSTRAINT service_delivery_recipient_not_blank CHECK (length(recipient) > 0),
    CONSTRAINT service_delivery_key_not_blank CHECK (length(idempotency_key) > 0),
    CONSTRAINT service_delivery_digest_sha256 CHECK (body_digest ~ '^[0-9a-f]{64}$')
);
