-- Keyless agents (DF-CRIER-192): registration's public_key requirement is
-- conditional on signature enforcement (CR_REQUIRE_AGENT_SIG), so an agent
-- registered while enforcement is off may carry no key at all. NULL is the
-- canonical "no key" value (the store writes a nil slice, which pgx encodes
-- as NULL); any non-null value must still be exactly one ed25519 public key.
ALTER TABLE agents
    ALTER COLUMN public_key DROP NOT NULL,
    DROP CONSTRAINT agents_public_key_ed25519_length,
    ADD CONSTRAINT agents_public_key_ed25519_length
        CHECK (public_key IS NULL OR octet_length(public_key) = 32);
