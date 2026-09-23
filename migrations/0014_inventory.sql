-- Stock.
--
-- Quantity on hand is the sum of movements, never a column somebody writes
-- over. A number that is written over cannot answer "how much did we lose to
-- wrongly ordered parts this year", and that is one of the few figures a
-- workshop owner can actually act on.
--
-- It is the same principle as an immutable invoice, for the same reason: it
-- makes "where did it go" answerable after the fact.

-- Where a part lives, so somebody can be sent to fetch it.
ALTER TABLE parts ADD COLUMN location text;
ALTER TABLE parts ADD COLUMN minimum_quantity numeric(12,3) NOT NULL DEFAULT 0
    CHECK (minimum_quantity >= 0);

-- A part is one thing under several supplier numbers, and a barcode belongs to
-- the packaging rather than to the part. Many codes map onto one part; a part
-- is not identified by a single code.
CREATE TABLE part_codes (
    id         uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    part_id    uuid    NOT NULL REFERENCES parts(id) ON DELETE CASCADE,
    code       text    NOT NULL CHECK (length(trim(code)) > 0),
    kind       text    NOT NULL DEFAULT 'barcode'
               CHECK (kind IN ('barcode', 'supplier', 'oem', 'shop')),
    supplier   text,
    UNIQUE (shop_id, code)
);
CREATE INDEX part_codes_part_idx ON part_codes (part_id);

-- The ledger.
--
-- Reserved is neither sold nor free. A part put aside for a job that is then
-- declined has to stop being reserved, and modelling that as "reduce the
-- number" loses the fact that it happened.
CREATE TABLE stock_movements (
    id       uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id  uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    part_id  uuid    NOT NULL REFERENCES parts(id) ON DELETE RESTRICT,

    kind     text    NOT NULL CHECK (kind IN (
                 'received',      -- arrived from a supplier
                 'reserved',      -- put aside for a job
                 'unreserved',    -- the job was declined or changed
                 'consumed',      -- fitted to a car
                 'returned',      -- went back to the supplier
                 'written_off',   -- see reason
                 'counted'        -- a stocktake correction
             )),

    -- Signed, in the part's own unit. Oil is not a count, so this is a decimal.
    quantity numeric(12,3) NOT NULL CHECK (quantity <> 0),

    -- Only on a write-off, and required there. A flag on the part could say
    -- "this is gone"; only a reason can say what a year of wrong orders cost.
    reason   text CHECK (reason IN (
                 'wrong_part_ordered', 'damaged', 'opened_not_returnable',
                 'obsolete', 'lost', 'warranty_scrap')),

    -- What it cost at the moment it moved. A part fitted last year was billed
    -- at last year's price, and a valuation that reprices history is a
    -- valuation nobody can reconcile.
    unit_cost_minor bigint,

    work_order_id uuid REFERENCES work_orders(id) ON DELETE RESTRICT,
    line_id       uuid REFERENCES work_order_lines(id) ON DELETE SET NULL,
    moved_by      uuid REFERENCES users(id) ON DELETE RESTRICT,
    moved_at      timestamptz NOT NULL DEFAULT now(),
    note          text,

    CHECK ((kind = 'written_off') = (reason IS NOT NULL))
);
CREATE INDEX stock_movements_part_idx ON stock_movements (part_id, moved_at DESC);
CREATE INDEX stock_movements_order_idx ON stock_movements (work_order_id)
    WHERE work_order_id IS NOT NULL;
CREATE INDEX stock_movements_writeoff_idx ON stock_movements (shop_id, moved_at)
    WHERE kind = 'written_off';

-- Cost bands to markup, so a shop prices consistently without typing a
-- percentage on every line.
--
-- Bands rather than one percentage: a five-krona clip and a five-thousand
-- krona turbo do not carry the same markup, and every workshop knows it.
CREATE TABLE price_bands (
    id             uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id        uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    up_to_minor    bigint  CHECK (up_to_minor IS NULL OR up_to_minor > 0),
    markup_basis   integer NOT NULL CHECK (markup_basis >= 0 AND markup_basis <= 100000),
    UNIQUE (shop_id, up_to_minor)
);
COMMENT ON COLUMN price_bands.up_to_minor IS
    'Upper bound of the cost band, exclusive. NULL is the open-ended top band.';

DO $$
DECLARE t text;
BEGIN
    FOR t IN SELECT unnest(ARRAY['part_codes', 'stock_movements', 'price_bands'])
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$
            CREATE POLICY shop_isolation ON %I
                USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
                WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
        $p$, t);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO redasms_app', t);
    END LOOP;
END
$$;
