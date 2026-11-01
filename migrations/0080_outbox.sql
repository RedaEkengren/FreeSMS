-- rollback: safe
-- New tables the previous release neither reads nor writes.
--
-- Every call to the outside world -- a signing request, an ownership change
-- filed, an ad pushed, a payment provider asked -- is a row here, written in
-- the same transaction as the change that caused it, and sent afterwards by
-- a worker (#154). A change rolled back leaves no row; a row is sent with
-- its own idempotency key, so a send retried after a timeout, a crash or a
-- restart is the same request to the provider, and in effect happens once.
--
-- The workshop's customer messages keep their own outbox (0040), as they
-- are: moving them here is a change of its own, if it is ever made.
CREATE TABLE outbox (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id         uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    kind            text        NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_]{0,59}$'),
    subject_kind    text        CHECK (subject_kind IS NULL OR subject_kind ~ '^[a-z][a-z_]{0,39}$'),
    subject_id      uuid,
    -- The module whose business it is, if any: switched off, its calls not
    -- yet made are cancelled with the reason.
    module          text        CHECK (module IS NULL OR module IN ('workshop', 'dealer', 'rental')),
    -- Ids and amounts, the least a sender needs: what it says about a
    -- person it reads when it sends, so an erasure in between is honoured.
    -- Never a key or a token.
    payload         jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(payload) = 'object'),
    idempotency_key text        NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    not_before      timestamptz NOT NULL DEFAULT clock_timestamp(),
    max_attempts    integer     NOT NULL DEFAULT 8 CHECK (max_attempts BETWEEN 1 AND 50),
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by      uuid        REFERENCES users(id) ON DELETE RESTRICT,
    -- The worker's lease: a row being sent is not sent again by another
    -- worker until this passes, which it does if the first one died.
    claimed_until   timestamptz,
    CHECK ((subject_kind IS NULL) = (subject_id IS NULL)),
    UNIQUE (shop_id, kind, idempotency_key)
);
CREATE INDEX outbox_subject_idx ON outbox (subject_id) WHERE subject_id IS NOT NULL;

-- What happened to each, as rows: where it stands is its latest.
CREATE TABLE outbox_events (
    id        uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id   uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    outbox_id uuid        NOT NULL REFERENCES outbox(id) ON DELETE RESTRICT,
    seq       bigint      GENERATED ALWAYS AS IDENTITY,
    event     text        NOT NULL CHECK (event IN ('queued', 'attempted', 'succeeded', 'failed', 'retry_scheduled',
                                                    'given_up', 'cancelled', 'manual_task', 'done_by_hand')),
    at        timestamptz NOT NULL DEFAULT clock_timestamp(),
    -- When the next attempt is due, for a retry scheduled.
    retry_at  timestamptz,
    -- What went wrong, as the provider said it, without a key or a token;
    -- why it was cancelled.
    detail    text        CHECK (detail IS NULL OR length(detail) <= 500),
    -- Null for the worker.
    by_user   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((event = 'retry_scheduled') = (retry_at IS NOT NULL))
);
CREATE INDEX outbox_events_outbox_idx ON outbox_events (outbox_id, seq);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['outbox', 'outbox_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        -- The default privileges (0018) give everything; revoke first (0071).
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END;
$$;
-- The lease is the one thing about a row that changes.
GRANT UPDATE (claimed_until) ON outbox TO freesms_app;

-- Where each row stands: its latest event, how many attempts it has had,
-- and when it is next due, if it is.
CREATE VIEW outbox_state WITH (security_invoker = true) AS
    SELECT o.id, o.shop_id, o.kind, o.subject_kind, o.subject_id, o.idempotency_key, o.not_before, o.max_attempts,
           o.claimed_until, o.created_at, l.event AS state, l.at AS state_at, l.detail,
           -- Since it was last queued: a person's retry starts them again.
           (SELECT count(*) FROM outbox_events a WHERE a.outbox_id = o.id AND a.event = 'attempted'
              AND a.seq > (SELECT max(q.seq) FROM outbox_events q WHERE q.outbox_id = o.id AND q.event = 'queued'))::int AS attempts,
           CASE WHEN l.event = 'queued' THEN o.not_before
                WHEN l.event = 'retry_scheduled' THEN l.retry_at
                -- An attempt whose worker died: due again once its lease is
                -- past, with the same key.
                WHEN l.event = 'attempted' THEN coalesce(o.claimed_until, l.at) END AS due_at
      FROM outbox o
      JOIN LATERAL (SELECT e.event, e.at, e.retry_at, e.detail FROM outbox_events e
                     WHERE e.outbox_id = o.id ORDER BY e.seq DESC LIMIT 1) l ON true;
GRANT SELECT ON outbox_state TO freesms_app;
