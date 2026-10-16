-- rollback: safe
-- New tables the previous release never reads. It would let a car be ready
-- with a gearbox still at the specialist, which is how it always behaved.
--
-- Work sent to somebody else. A sublet line says what the customer is charged;
-- it cannot say that the turbocharger left on Tuesday, was promised for
-- Friday, came back with a report, and failed the shop's own check. That
-- lives between the shop's tasks and is lost there.
--
-- Everything that can happen twice is a row. Where the work stands is the
-- latest of the status events; who is chasing it is the latest assignment;
-- what it cost is the latest cost; the supplier's invoice is a row, and
-- handing it to the accounts is another. Receipt from a supplier is not
-- acceptance: 'returned' and 'checked' are different events by different
-- people, and the second is the shop's own.
CREATE TABLE outsourced_work (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id        uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id  uuid        NOT NULL REFERENCES work_orders(id) ON DELETE RESTRICT,
    -- The operation it is the work for, when there is a priced one.
    line_id        uuid        REFERENCES work_order_lines(id) ON DELETE RESTRICT,
    supplier       text        NOT NULL CHECK (length(trim(supplier)) BETWEEN 1 AND 100),
    contact        text        CHECK (contact IS NULL OR length(contact) <= 200),
    scope          text        NOT NULL CHECK (length(trim(scope)) BETWEEN 1 AND 500),
    -- Their reference, an order or RMA number.
    external_ref   text        CHECK (external_ref IS NULL OR length(external_ref) <= 100),
    -- What physically goes: a part off the car, or the car.
    item           text        NOT NULL CHECK (length(trim(item)) BETWEEN 1 AND 100),
    item_kind      text        NOT NULL CHECK (item_kind IN ('component', 'vehicle')),
    expected_back  date,
    -- Agreed or estimated, before VAT.
    estimate_minor bigint      CHECK (estimate_minor IS NULL OR estimate_minor >= 0),
    -- A job with required outsourced work that is not back and checked is not
    -- ready. Not required: the shop may go on without it.
    required       boolean     NOT NULL DEFAULT true,
    created_by     uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at     timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX outsourced_work_order_idx ON outsourced_work (work_order_id);

CREATE TABLE outsourced_events (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    outsourced_id uuid        NOT NULL REFERENCES outsourced_work(id) ON DELETE RESTRICT,
    kind          text        NOT NULL CHECK (kind IN (
                                  'requested', 'sent', 'waiting', 'returned', 'checked', 'check_failed', 'cancelled',
                                  'assigned', 'cost', 'invoice', 'reconciled', 'note')),
    note          text        CHECK (note IS NULL OR length(note) <= 500),
    -- For 'waiting' and 'sent': when it is now expected.
    expected_back date,
    -- For 'returned': what came of it. Nothing done, a replacement unit and
    -- a diagnosis alone are all real, and billable, results.
    outcome       text        CHECK (outcome IN ('repaired', 'replaced', 'no_work', 'diagnosis_only')),
    -- For 'cost' and 'invoice': what it came to, before VAT.
    amount_minor  bigint      CHECK (amount_minor IS NULL OR amount_minor >= 0),
    -- For 'invoice': the supplier's own number for it.
    invoice_ref   text        CHECK (invoice_ref IS NULL OR length(invoice_ref) <= 100),
    -- For 'assigned': who is chasing it.
    assignee      uuid        REFERENCES users(id) ON DELETE RESTRICT,
    by_user       uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((kind = 'returned') = (outcome IS NOT NULL)),
    CHECK (kind = 'invoice' OR ((kind = 'cost') = (amount_minor IS NOT NULL))),
    CHECK ((kind = 'invoice') = (invoice_ref IS NOT NULL)),
    CHECK ((kind = 'assigned') = (assignee IS NOT NULL)),
    -- Said by somebody, with a reason.
    CHECK (kind NOT IN ('returned', 'check_failed', 'cancelled', 'note') OR length(trim(coalesce(note, ''))) > 0)
);
CREATE INDEX outsourced_events_idx ON outsourced_events (outsourced_id, at);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['outsourced_work', 'outsourced_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END $$;
