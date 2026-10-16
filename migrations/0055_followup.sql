-- rollback: safe
-- New tables the previous release never reads, and wider lists of what a
-- message can be about and what can happen to one, which it never writes.
--
-- A balance is a number. Chasing it is a conversation that goes on over
-- weeks: who has it, when they will ring next, what the customer said, what
-- they promised, whether they dispute the charge. None of that belongs in the
-- balance, which stays what the invoice, its credit notes and its payments
-- say; it belongs beside it, as rows.

ALTER TABLE message_events DROP CONSTRAINT IF EXISTS message_events_event_check;
-- 'cancelled': the message was never sent because what it was about stopped
-- being true while it waited -- a reminder for an invoice that was paid.
ALTER TABLE message_events ADD CONSTRAINT message_events_event_check
    CHECK (event IN ('queued', 'sent', 'delivered', 'failed', 'retry', 'cancelled'));

ALTER TABLE outbound_messages DROP CONSTRAINT IF EXISTS outbound_messages_about_check;
ALTER TABLE outbound_messages ADD CONSTRAINT outbound_messages_about_check
    CHECK (about IN ('ready', 'answer', 'waiting_part', 'invoice', 'reminder'));
ALTER TABLE customer_contacts DROP CONSTRAINT IF EXISTS customer_contacts_about_check;
ALTER TABLE customer_contacts ADD CONSTRAINT customer_contacts_about_check
    CHECK (about IN ('ready', 'answer', 'waiting_part', 'invoice', 'reminder'));

-- The invoice a message is a reminder for, so that the sender can ask whether
-- it is still owed at the moment it would go and not only when it was queued.
ALTER TABLE outbound_messages ADD COLUMN reminder_for uuid REFERENCES invoices(id) ON DELETE RESTRICT;

-- Who has an invoice that is owed or owed back, and when they will look at it
-- next. One per invoice, made when somebody first takes it.
CREATE TABLE receivable_cases (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    invoice_id    uuid        NOT NULL UNIQUE REFERENCES invoices(id) ON DELETE RESTRICT,
    owner_id      uuid        REFERENCES users(id) ON DELETE RESTRICT,
    next_follow_up date,
    -- The customer disputes the charge. It changes nothing in the balance and
    -- stops reminders going out while it stands.
    disputed_at   timestamptz,
    dispute_note  text        CHECK (dispute_note IS NULL OR length(dispute_note) <= 300),
    updated_at    timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- What was done and said, in order.
CREATE TABLE receivable_events (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    invoice_id    uuid        NOT NULL REFERENCES invoices(id) ON DELETE RESTRICT,
    kind          text        NOT NULL CHECK (kind IN ('taken', 'contact', 'promise', 'dispute', 'dispute_resolved', 'reminder', 'refund')),
    -- Phoned, wrote, visited, or another way.
    how           text        CHECK (how IS NULL OR length(how) <= 40),
    note          text        CHECK (note IS NULL OR length(note) <= 500),
    -- For a promise: how much, by when, and what was owed when it was made,
    -- so that keeping it can be read from what was paid afterwards.
    promised_minor bigint     CHECK (promised_minor IS NULL OR promised_minor > 0),
    promised_by   date,
    owed_then_minor bigint,
    message_id    uuid        REFERENCES outbound_messages(id) ON DELETE RESTRICT,
    payment_id    uuid        REFERENCES invoice_payments(id) ON DELETE RESTRICT,
    by_user       uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((kind = 'promise') = (promised_minor IS NOT NULL AND promised_by IS NOT NULL))
);
CREATE INDEX receivable_events_invoice_idx ON receivable_events (invoice_id, at);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['receivable_cases', 'receivable_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END $$;
GRANT UPDATE (owner_id, next_follow_up, disputed_at, dispute_note, updated_at) ON receivable_cases TO freesms_app;
