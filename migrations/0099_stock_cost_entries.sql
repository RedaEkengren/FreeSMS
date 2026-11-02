-- rollback: safe
-- New tables, and a trigger on outsourced_events that writes to them only
-- when the job's car is in the dealer's stock. Nothing puts a car in stock
-- but this release's car sales, so for the previous release, as for a
-- workshop-only shop, the trigger finds no stock unit and does nothing.
--
-- Every krona spent on a stock car (#164): reconditioning, workshop work,
-- parts, transport, inspection, registration fees, advertising. They make
-- the business margin true (#182). They are never part of the tax purchase
-- price the margin scheme reads (#161): under the scheme, repairs and
-- transport are not part of what the car was bought for, and the VAT on
-- them is deducted as usual. Nothing here adds them to it.
--
-- Append-only: a correction is an entry reversing another, a supplier's
-- credit note an entry below nothing.
CREATE TABLE stock_cost_entries (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id            uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    stock_unit_id      uuid        NOT NULL REFERENCES stock_units(id) ON DELETE RESTRICT,
    kind               text        NOT NULL CHECK (kind IN ('reconditioning', 'workshop', 'parts', 'transport', 'inspection',
                                                            'registration', 'advertising', 'other')),
    -- In kronor, minor units; below nothing for a credit.
    net_minor          bigint      NOT NULL CHECK (net_minor BETWEEN -10000000000 AND 10000000000),
    input_vat_minor    bigint      NOT NULL DEFAULT 0 CHECK (input_vat_minor BETWEEN -10000000000 AND 10000000000),
    vat_rate_basis     integer     NOT NULL DEFAULT 2500 CHECK (vat_rate_basis BETWEEN 0 AND 10000),
    supplier_party_id  uuid        REFERENCES parties(id) ON DELETE RESTRICT,
    external_ref       text        CHECK (external_ref IS NULL OR length(external_ref) <= 100),
    incurred_on        date        NOT NULL,
    source             text        NOT NULL CHECK (source IN ('work_order', 'outsourced', 'supplier_invoice', 'manual')),
    work_order_id      uuid        REFERENCES work_orders(id) ON DELETE RESTRICT,
    outsourced_event_id uuid       UNIQUE REFERENCES outsourced_events(id) ON DELETE RESTRICT,
    -- The moment the cost is for, which says whether it is after the sale
    -- (stock_cost_post_sale, below): when the job it came from was opened,
    -- for a job's posting and a supplier's invoice for work sent out from
    -- it; the end of incurred_on, or when it was recorded if earlier, for
    -- a cost dated by hand or from an invoice. A reversal copies the moment of the entry it reverses; an
    -- invoice that replaces another is for the same job's work, and so for
    -- the same moment.
    happened_at        timestamptz NOT NULL,
    -- An invoice in another currency: what it said, and the rate it was
    -- turned into kronor at, in millionths, and from where.
    currency           char(3)     CHECK (currency IS NULL OR (currency ~ '^[A-Z]{3}$' AND currency <> 'SEK')),
    original_net_minor bigint,
    rate_micro         bigint      CHECK (rate_micro IS NULL OR rate_micro BETWEEN 1 AND 100000000),
    rate_source        text        CHECK (rate_source IS NULL OR length(btrim(rate_source)) BETWEEN 1 AND 200),
    note               text        CHECK (note IS NULL OR length(note) <= 500),
    reverses_id        uuid        UNIQUE REFERENCES stock_cost_entries(id) ON DELETE RESTRICT,
    recorded_at        timestamptz NOT NULL DEFAULT clock_timestamp(),
    recorded_by        uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((currency IS NULL) = (original_net_minor IS NULL)
       AND (currency IS NULL) = (rate_micro IS NULL)
       AND (currency IS NULL) = (rate_source IS NULL)),
    CHECK ((source = 'outsourced') = (outsourced_event_id IS NOT NULL)),
    CHECK (source <> 'work_order' OR work_order_id IS NOT NULL)
);
CREATE INDEX stock_cost_entries_unit_idx ON stock_cost_entries (stock_unit_id, incurred_on);
-- A job's posting at cost, found again rather than made twice.
CREATE INDEX stock_cost_entries_job_idx ON stock_cost_entries (work_order_id) WHERE work_order_id IS NOT NULL;

ALTER TABLE stock_cost_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE stock_cost_entries FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON stock_cost_entries
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON stock_cost_entries FROM freesms_app;
GRANT SELECT, INSERT ON stock_cost_entries TO freesms_app;

-- The rate the workshop's own hours on a stock car are costed at: the
-- shop's cost of an hour, not what a customer pays for one. A setting
-- changed is a row.
CREATE TABLE stock_cost_settings (
    id                         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id                    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    seq                        bigint      GENERATED ALWAYS AS IDENTITY,
    internal_labour_rate_minor bigint      NOT NULL CHECK (internal_labour_rate_minor BETWEEN 0 AND 10000000),
    changed_at                 timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by                 uuid        REFERENCES users(id) ON DELETE RESTRICT
);

ALTER TABLE stock_cost_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE stock_cost_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON stock_cost_settings
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON stock_cost_settings FROM freesms_app;
GRANT SELECT, INSERT ON stock_cost_settings TO freesms_app;

-- Whether a cost is after the sale -- a complaint repaired: it changes the
-- business margin, never the VAT. Decided by what the cost is for, not by
-- when it was posted, and read whenever a cost is read, never stored:
--
--   * A cost is after the sale when the moment it is for (happened_at) is
--     later than the sale that stands (stock_units.sold_at). Work on a job
--     opened before the sale is before it however late its invoice is
--     posted; a job opened after the sale is after it.
--   * A cost dated rather than timed is for the end of its day, or the
--     moment it was recorded if that is earlier, or the start of its day
--     if that is still to come (stock_cost_dated): one dated before the day
--     of the sale is before it, one dated a later day is after it, and on
--     the day of the sale itself, one entered before the sale was made is
--     before it and one entered after is after it. A cost dated the day of
--     the sale and entered on a later day is after it: what is not known
--     to be before the sale is not counted in what the car cost to sell.
--   * A sale that falls through (sold back to for sale) clears sold_at:
--     it was never a sale, and every cost is before the next one.
--   * A reversal, and a supplier's invoice that replaces another, carry
--     the moment of the entry they stand in for (the replacement because it
--     is for the same job), so they are always on its side of the sale.
--
-- A cycle closed without a sale -- a consigned car handed back, a purchase
-- undone -- or written off has no sale to be after. One rule, read by the
-- application wherever a cost is read.
CREATE FUNCTION stock_cost_post_sale(unit uuid, happened timestamptz) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT coalesce(happened > sold_at, false) FROM stock_units WHERE id = unit
$$;

-- The moment a cost dated by hand is for: the end of its day where the
-- shop is, or now if that is earlier -- unless its day has not begun yet,
-- when it is the start of that day. A cost entered this morning for
-- tomorrow is for tomorrow, and so after a sale made this afternoon, as
-- its date says; taking now would put it before.
CREATE FUNCTION stock_cost_dated(shop uuid, day date) RETURNS timestamptz LANGUAGE sql VOLATILE AS $$
    SELECT CASE WHEN day::timestamp AT TIME ZONE tz > clock_timestamp() THEN day::timestamp AT TIME ZONE tz
                ELSE least(clock_timestamp(), (day + 1)::timestamp AT TIME ZONE tz) END
      FROM (SELECT timezone AS tz FROM shops WHERE id = shop) s
$$;

-- A supplier's invoice for work sent out from a job on a car in stock is
-- the car's cost, posted in the same transaction as the invoice is
-- recorded, whichever release records it: kind workshop, as it is work
-- done on the car, with the VAT a Swedish supplier charges on it. A job
-- opened while the car was not in stock -- every customer's car, the car
-- after it was sold -- posts nothing. It is for the moment the job was
-- opened, and so on that side of the sale.
CREATE FUNCTION outsourced_invoice_costs_stock() RETURNS trigger AS $$
DECLARE
    unit uuid;
    happened timestamptz;
BEGIN
    IF NEW.kind <> 'invoice' THEN
        RETURN NEW;
    END IF;
    -- An invoice noted again for the same work replaces the one before,
    -- as the workshop reads it (the latest stands): the cost posted from
    -- the earlier one is reversed, with the new invoice's reference as
    -- why, rather than counted twice. So is one noted without an amount
    -- yet: the earlier invoice no longer stands, and its cost goes with
    -- it until the new one's amount is noted. Each entry is locked first
    -- the way ReverseStockCost locks it, so a reversal by hand at the same
    -- moment waits and finds it reversed, rather than both reversing it;
    -- the INSERT below then reads what the wait let commit. The job comes
    -- first, then the costs, the order ReverseStockCost takes them in:
    -- the reversal's foreign key needs a share of the job, so a lock on
    -- the cost taken before the job would wait on a page that holds the
    -- job and waits on the cost.
    PERFORM 1 FROM work_orders w JOIN outsourced_work o ON o.work_order_id = w.id
      WHERE o.id = NEW.outsourced_id FOR SHARE OF w;
    PERFORM pg_advisory_xact_lock(hashtextextended('cost:' || e.id::text, 0))
       FROM stock_cost_entries e
       JOIN outsourced_events oe ON oe.id = e.outsourced_event_id
      WHERE oe.outsourced_id = NEW.outsourced_id
      ORDER BY e.id;
    INSERT INTO stock_cost_entries (shop_id, stock_unit_id, kind, net_minor, input_vat_minor, vat_rate_basis,
                                    supplier_party_id, external_ref, incurred_on, source, work_order_id, happened_at,
                                    note, reverses_id, recorded_by)
    SELECT e.shop_id, e.stock_unit_id, e.kind, -e.net_minor, -e.input_vat_minor, e.vat_rate_basis, e.supplier_party_id,
           e.external_ref, (NEW.at AT TIME ZONE (SELECT timezone FROM shops WHERE id = NEW.shop_id))::date, 'manual',
           e.work_order_id, e.happened_at, NEW.invoice_ref, e.id, NEW.by_user
      FROM stock_cost_entries e
      JOIN outsourced_events oe ON oe.id = e.outsourced_event_id
     WHERE oe.outsourced_id = NEW.outsourced_id
       AND NOT EXISTS (SELECT 1 FROM stock_cost_entries r WHERE r.reverses_id = e.id);
    IF NEW.amount_minor IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT u.id, w.opened_at INTO unit, happened
      FROM outsourced_work o
      JOIN work_orders w ON w.id = o.work_order_id
      JOIN stock_units u ON u.vehicle_id = w.vehicle_id AND u.opened_at <= w.opened_at
                         AND (u.closed_at IS NULL OR u.closed_at > w.opened_at)
     WHERE o.id = NEW.outsourced_id;
    IF unit IS NULL THEN
        RETURN NEW;
    END IF;
    INSERT INTO stock_cost_entries (shop_id, stock_unit_id, kind, net_minor, input_vat_minor, incurred_on, source,
                                    work_order_id, outsourced_event_id, external_ref, happened_at, recorded_by)
    SELECT NEW.shop_id, unit, 'workshop', NEW.amount_minor, round(NEW.amount_minor * 0.25)::bigint,
           (NEW.at AT TIME ZONE (SELECT timezone FROM shops WHERE id = NEW.shop_id))::date, 'outsourced',
           o.work_order_id, NEW.id, NEW.invoice_ref, happened, NEW.by_user
      FROM outsourced_work o WHERE o.id = NEW.outsourced_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER outsourced_invoice_costs_stock AFTER INSERT ON outsourced_events
    FOR EACH ROW EXECUTE FUNCTION outsourced_invoice_costs_stock();
