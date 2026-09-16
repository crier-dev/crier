ALTER TABLE agents
    ALTER COLUMN public_key SET NOT NULL,
    DROP CONSTRAINT agents_public_key_ed25519_length,
    ADD CONSTRAINT agents_public_key_ed25519_length
        CHECK (octet_length(public_key) = 32);
