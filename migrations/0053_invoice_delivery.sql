-- rollback: safe
-- New tables the previous release never reads, and one more thing a message
-- can be about that it never writes.
--
-- An invoice is a document the customer is owed, not only a record the
-- workshop keeps. Getting it to them is a sequence of events, each of which
-- can fail, and "the provider accepted it" is not "the customer has it". So
-- each attempt is a row, the way each message event already is, and the one
-- thing that does show they have it -- opening the link -- is a row too.

ALTER TABLE outbound_messages DROP CONSTRAINT IF EXISTS outbound_messages_about_check;
ALTER TABLE outbound_messages ADD CONSTRAINT outbound_messages_about_check
    CHECK (about IN ('ready', 'answer', 'waiting_part', 'invoice'));

ALTER TABLE customer_contacts DROP CONSTRAINT IF EXISTS customer_contacts_about_check;
ALTER TABLE customer_contacts ADD CONSTRAINT customer_contacts_about_check
    CHECK (about IN ('ready', 'answer', 'waiting_part', 'invoice'));

-- A link to one document, and no other. It opens that invoice or credit note
-- and nothing of the staff's pages, the job or the car's history. A new link
-- for the same document closes the earlier one.
CREATE TABLE invoice_links (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id        uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    invoice_id     uuid        NOT NULL REFERENCES invoices(id) ON DELETE RESTRICT,
    token_sha256   bytea       NOT NULL UNIQUE,
    created_by     uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at     timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at     timestamptz NOT NULL,
    revoked_at     timestamptz,
    revoked_by     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    first_opened_at timestamptz,
    last_opened_at timestamptz,
    opens          integer     NOT NULL DEFAULT 0,
    CHECK (expires_at > created_at),
    CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);
CREATE INDEX invoice_links_invoice_idx ON invoice_links (invoice_id);

-- Each way the document was put in somebody's hands: sent as a message with a
-- link, or given over at the counter, in the post, printed.
CREATE TABLE invoice_deliveries (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id        uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    invoice_id     uuid        NOT NULL REFERENCES invoices(id) ON DELETE RESTRICT,
    kind           text        NOT NULL CHECK (kind IN ('message', 'handed')),
    -- Who it was meant for, as the desk confirmed: which can be someone other
    -- than the vehicle's owner or the person who booked.
    recipient_name text        NOT NULL CHECK (length(trim(recipient_name)) BETWEEN 1 AND 100),
    message_id     uuid        REFERENCES outbound_messages(id) ON DELETE RESTRICT,
    link_id        uuid        REFERENCES invoice_links(id) ON DELETE RESTRICT,
    -- For a handover: printed at the counter, handed over, posted.
    how            text        CHECK (how IN ('printed', 'handed', 'posted')),
    note           text        CHECK (note IS NULL OR length(note) <= 300),
    by_user        uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at             timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((kind = 'message') = (message_id IS NOT NULL)),
    CHECK ((kind = 'handed') = (how IS NOT NULL))
);
CREATE INDEX invoice_deliveries_invoice_idx ON invoice_deliveries (invoice_id, at);

ALTER TABLE invoice_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_links FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON invoice_links
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON invoice_links FROM freesms_app;
GRANT SELECT, INSERT ON invoice_links TO freesms_app;
GRANT UPDATE (revoked_at, revoked_by, first_opened_at, last_opened_at, opens) ON invoice_links TO freesms_app;

ALTER TABLE invoice_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON invoice_deliveries
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON invoice_deliveries FROM freesms_app;
GRANT SELECT, INSERT ON invoice_deliveries TO freesms_app;
-- Erasing a person blanks the name it was meant for.
GRANT UPDATE (recipient_name) ON invoice_deliveries TO freesms_app;

CREATE TRIGGER notify_change AFTER INSERT OR UPDATE ON invoice_deliveries
    FOR EACH ROW EXECUTE FUNCTION notify_change('job');
