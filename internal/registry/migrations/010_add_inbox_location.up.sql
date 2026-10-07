-- CR-CHAT-029: persist the machine-readable delivery LOCATION.
--
-- A delivery now states WHERE it landed — instance, namespace, channel
-- (the session), thread and sub-thread (specs/CHAT-SESSIONS.md §4.5) — so
-- the agent can SAY where it is instead of inferring it from prose. The
-- registry deliver path derives the location from the request context and
-- the target's resolved realm and records it WITH the message.
--
-- JSONB and NULLABLE with no default: a pre-existing row reads back as
-- NULL ("not recorded"), never an invented value, exactly the posture the
-- thread_id column (009, CR-CHAT-019) took. The shape is the API's
-- contract (registry.Location), not something this table enforces.
ALTER TABLE inbox_entries
    ADD COLUMN location JSONB NULL;
