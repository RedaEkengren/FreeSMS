-- rollback: safe
-- A new table the previous release neither reads nor writes.
--
-- A rental invoiced (#199): which of the booking's frozen lines and which
-- confirmed return charges each invoice charges for. Each is invoiced at
-- most once while that invoice stands. A credit note releases what its
-- invoice charged, and charged again it is a new round: the unique index
-- on (item, round) is what makes two requests at once produce one invoice,
-- whatever the code above it does.
CREATE TABLE rental_invoice_items (
    shop_id         uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    invoice_id      uuid    NOT NULL REFERENCES invoices(id) ON DELETE RESTRICT,
    booking_line_id uuid    REFERENCES rental_booking_lines(id) ON DELETE RESTRICT,
    charge_id       uuid    REFERENCES rental_charges(id) ON DELETE RESTRICT,
    -- How many invoices of it were credited before this one.
    round           integer NOT NULL CHECK (round >= 0),
    CHECK (num_nonnulls(booking_line_id, charge_id) = 1)
);
CREATE UNIQUE INDEX rental_invoice_items_line_idx ON rental_invoice_items (booking_line_id, round) WHERE booking_line_id IS NOT NULL;
CREATE UNIQUE INDEX rental_invoice_items_charge_idx ON rental_invoice_items (charge_id, round) WHERE charge_id IS NOT NULL;
CREATE INDEX rental_invoice_items_invoice_idx ON rental_invoice_items (invoice_id);

ALTER TABLE rental_invoice_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE rental_invoice_items FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON rental_invoice_items
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON rental_invoice_items FROM freesms_app;
GRANT SELECT, INSERT ON rental_invoice_items TO freesms_app;

