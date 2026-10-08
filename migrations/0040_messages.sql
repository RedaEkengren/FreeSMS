-- rollback: safe
-- New tables, a nullable column, and a CHECK relaxed. The previous release
-- never reads the tables; it reads a contact with a message on it as the
-- text or email it records, and filters what it shows to about = 'ready'.
--
-- Telling the customer from the system, by email or text, through whichever
-- provider the workshop has. A message is queued, then sent when it may be
-- -- not in the evening, not twice -- and what happened to it is a row each
-- time something did.

-- What a message is about: the car is ready; we need your answer; we are
-- waiting for a part. One of each that matters, not one per state change.
ALTER TABLE customer_contacts DROP CONSTRAINT customer_contacts_about_check;
ALTER TABLE customer_contacts ADD CONSTRAINT customer_contacts_about_check
    CHECK (about IN ('ready', 'answer', 'waiting_part'));

CREATE TABLE outbound_messages (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE RESTRICT,
    about         text        NOT NULL CHECK (about IN ('ready', 'answer', 'waiting_part')),
    channel       text        NOT NULL CHECK (channel IN ('sms', 'email')),
    -- An E.164 number or an address. Cleared, and only this, when the
    -- customer is erased: it is theirs, and nothing requires keeping it.
    recipient     text        NOT NULL,
    subject       text        NOT NULL DEFAULT '',
    body          text        NOT NULL CHECK (length(body) BETWEEN 1 AND 1600),
    -- Not before this: a message queued in the evening waits for the morning.
    not_before    timestamptz NOT NULL,
    created_by    uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX outbound_messages_order_idx ON outbound_messages (work_order_id, created_at);

-- What happened to a message, in order: queued, sent (handed to the
-- provider), delivered or failed (where the provider says), retry (it could
-- not be handed over and will be tried again).
CREATE TABLE message_events (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    message_id  uuid        NOT NULL REFERENCES outbound_messages(id) ON DELETE RESTRICT,
    event       text        NOT NULL CHECK (event IN ('queued', 'sent', 'delivered', 'failed', 'retry')),
    provider    text,
    provider_id text,
    -- What went wrong, as the provider said it; never a key.
    detail      text        CHECK (detail IS NULL OR length(detail) <= 500),
    at          timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX message_events_message_idx ON message_events (message_id, at);
CREATE INDEX message_events_provider_idx ON message_events (provider, provider_id) WHERE provider_id IS NOT NULL;

-- A contact recorded by sending, rather than by somebody saying they did.
ALTER TABLE customer_contacts ADD COLUMN message_id uuid REFERENCES outbound_messages(id) ON DELETE RESTRICT;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['outbound_messages', 'message_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END
$$;
GRANT UPDATE (recipient) ON outbound_messages TO freesms_app;

-- A message's events belong to its job, one step away.
CREATE OR REPLACE FUNCTION notify_change() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    r     jsonb;
    topic text;
    job   text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        r := to_jsonb(OLD);
    ELSE
        r := to_jsonb(NEW);
    END IF;

    FOREACH topic IN ARRAY TG_ARGV LOOP
        IF topic <> 'job' THEN
            PERFORM pg_notify('freesms_changes', (r->>'shop_id') || ' ' || topic);
            CONTINUE;
        END IF;

        -- Which job, directly or one step away. Read under the same row
        -- level security as the write, so it can only find this shop's.
        job := CASE WHEN TG_TABLE_NAME = 'work_orders' THEN r->>'id' ELSE r->>'work_order_id' END;
        IF job IS NULL AND r ? 'inspection_id' THEN
            SELECT work_order_id::text INTO job FROM inspections WHERE id = (r->>'inspection_id')::uuid;
        ELSIF job IS NULL AND TG_TABLE_NAME = 'inspection_decisions' THEN
            SELECT i.work_order_id::text INTO job
            FROM inspection_items it JOIN inspections i ON i.id = it.inspection_id
            WHERE it.id = (r->>'item_id')::uuid;
        ELSIF job IS NULL AND r ? 'invoice_id' THEN
            SELECT work_order_id::text INTO job FROM invoices WHERE id = (r->>'invoice_id')::uuid;
        ELSIF job IS NULL AND r ? 'request_id' THEN
            SELECT work_order_id::text INTO job FROM part_requests WHERE id = (r->>'request_id')::uuid;
        ELSIF job IS NULL AND r ? 'message_id' THEN
            SELECT work_order_id::text INTO job FROM outbound_messages WHERE id = (r->>'message_id')::uuid;
        END IF;
        IF job IS NOT NULL THEN
            PERFORM pg_notify('freesms_changes', (r->>'shop_id') || ' job:' || job);
        END IF;
    END LOOP;
    RETURN NULL;
END
$$;

CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON outbound_messages
    FOR EACH ROW EXECUTE FUNCTION notify_change('job');
CREATE TRIGGER notify_change AFTER INSERT ON message_events
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
