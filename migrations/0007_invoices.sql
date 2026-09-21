-- Invoices.
--
-- An invoice is an accounting record before it is a row. Swedish bookkeeping
-- law requires it to be unalterable and retained for seven years, and the
-- numbering to run without gaps. Everything below follows from that rather
-- than from what would be convenient to query.

-- The counter, as a table rather than a sequence.
--
-- A sequence does not roll back. Allocate a number, fail to write the invoice,
-- and the number is spent -- a gap, in a series that is not allowed to have
-- any. A row taken with FOR UPDATE is allocated inside the same transaction as
-- the invoice, so a failure un-allocates it as well.
--
-- The cost is that concurrent issues serialise on this row. That is correct:
-- gap-free numbering is a serial thing, and a workshop issues tens of invoices
-- a day, not thousands a second.
CREATE TABLE invoice_series (
    shop_id     uuid   NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    series      text   NOT NULL,
    next_number bigint NOT NULL DEFAULT 1 CHECK (next_number > 0),
    PRIMARY KEY (shop_id, series)
);

ALTER TABLE invoice_series ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_series FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON invoice_series
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- The invoice, and a copy of everything it asserts.
--
-- The customer's name and address are copied rather than joined. An invoice
-- says what was true when it was issued: correcting a customer's address next
-- year must not silently reprint last year's invoice to a place they did not
-- live. The same reasoning is why the lines are copied below instead of being
-- read back from the work order, which can still change.
--
-- That snapshot is also what makes storing a rendered PDF unnecessary for now.
-- The usual argument for keeping one is that re-rendering from live data would
-- produce a different document under the same number; when the data is frozen,
-- rendering is deterministic and a PDF is a cache. A signed archival PDF is a
-- separate question, for whenever somebody actually needs to send one.
CREATE TABLE invoices (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    series        text        NOT NULL,
    number        bigint      NOT NULL,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE RESTRICT,

    -- Set on a credit note, naming what it reverses. A correction is never an
    -- edit of the document being corrected.
    credit_of_id  uuid        REFERENCES invoices(id) ON DELETE RESTRICT,

    issued_at     timestamptz NOT NULL DEFAULT now(),
    issued_by     uuid        REFERENCES users(id) ON DELETE RESTRICT,

    customer_name       text NOT NULL,
    customer_address    text,
    customer_org_number text,
    customer_vat_number text,

    currency    char(3) NOT NULL DEFAULT 'SEK',
    net_minor   bigint  NOT NULL,
    vat_minor   bigint  NOT NULL,
    gross_minor bigint  NOT NULL,

    CHECK (gross_minor = net_minor + vat_minor),
    UNIQUE (shop_id, series, number)
);
CREATE INDEX invoices_work_order_idx ON invoices (work_order_id);
CREATE INDEX invoices_credit_of_idx ON invoices (credit_of_id) WHERE credit_of_id IS NOT NULL;

ALTER TABLE invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoices FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON invoices
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

CREATE TABLE invoice_lines (
    id          uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    invoice_id  uuid    NOT NULL REFERENCES invoices(id) ON DELETE RESTRICT,
    position    integer NOT NULL,
    kind        text    NOT NULL,
    description text    NOT NULL,
    quantity    numeric(12,3) NOT NULL,
    unit_price_minor bigint  NOT NULL,
    vat_rate_bp      integer NOT NULL,
    -- Computed at issue and stored, so that the arithmetic on the document
    -- cannot change because the rounding rule did.
    net_minor   bigint NOT NULL,
    vat_minor   bigint NOT NULL,
    UNIQUE (invoice_id, position)
);
CREATE INDEX invoice_lines_invoice_idx ON invoice_lines (invoice_id);

ALTER TABLE invoice_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE invoice_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON invoice_lines
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON invoice_series, invoices, invoice_lines TO redasms_app;

-- Immutability, enforced by the database.
--
-- "We never update invoices" is a convention, and conventions are kept until
-- somebody is in a hurry at five to five. This refuses, so the correction has
-- to be a credit note, which is what the law wants anyway.
--
-- Deletion is refused for the same reason: a series with a hole in it is a
-- series that cannot be reconciled, and the hole is invisible afterwards.
CREATE OR REPLACE FUNCTION invoices_are_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION
        'an issued invoice cannot be % (%, invoice %); correct it with a credit note',
        lower(TG_OP), TG_TABLE_NAME, coalesce(OLD.id::text, '?')
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER invoices_immutable
    BEFORE UPDATE OR DELETE ON invoices
    FOR EACH ROW EXECUTE FUNCTION invoices_are_immutable();

CREATE TRIGGER invoice_lines_immutable
    BEFORE UPDATE OR DELETE ON invoice_lines
    FOR EACH ROW EXECUTE FUNCTION invoices_are_immutable();
