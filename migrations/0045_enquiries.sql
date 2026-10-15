-- rollback: safe
-- New tables, new nullable columns, and two NOT NULL constraints relaxed. The
-- previous release never reads the new tables or columns and always supplies
-- the job the relaxed columns used to require.
--
-- An enquiry is a customer asking what something would cost, before there is
-- a car in the workshop or a job to hang the answer on. It keeps what is
-- known and leaves the rest unresolved: a registration nobody has, a payer
-- nobody has named. Nothing about it says the car is here, that work was
-- done or that anything is owed.
--
-- The proposal made on it is a quote, the same numbered, frozen version as
-- on a job, and the answer to it is the same append-only decision. So the
-- quote tables now belong to a job or to an enquiry -- exactly one -- and the
-- customer's link may point at either.

CREATE TABLE enquiries (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id        uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- What the customer asked for, in their words.
    subject        text        NOT NULL CHECK (btrim(subject) <> ''),
    registration   text,
    vehicle_text   text,
    customer_name  text,
    customer_phone text,
    created_by     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    created_at     timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The draft: what will be put to the customer. A line is never edited; a
-- price or a quantity that changes is the old line removed and a new one
-- added, so the draft can be read back as it was.
CREATE TABLE enquiry_lines (
    id               uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id          uuid          NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    enquiry_id       uuid          NOT NULL REFERENCES enquiries(id) ON DELETE CASCADE,
    position         integer       NOT NULL,
    kind             text          NOT NULL,
    description      text          NOT NULL CHECK (btrim(description) <> ''),
    quantity         numeric(12,3) NOT NULL CHECK (quantity > 0),
    unit_price_minor bigint        NOT NULL CHECK (unit_price_minor >= 0),
    vat_rate_bp      integer       NOT NULL,
    removed_at       timestamptz,
    UNIQUE (enquiry_id, position)
);

ALTER TABLE job_links
    ALTER COLUMN work_order_id DROP NOT NULL,
    ADD COLUMN enquiry_id uuid REFERENCES enquiries(id) ON DELETE CASCADE,
    ADD CONSTRAINT job_links_one_subject CHECK ((work_order_id IS NULL) <> (enquiry_id IS NULL));
CREATE INDEX job_links_enquiry_idx ON job_links (enquiry_id) WHERE enquiry_id IS NOT NULL;

ALTER TABLE quotes
    ALTER COLUMN work_order_id DROP NOT NULL,
    ADD COLUMN enquiry_id  uuid REFERENCES enquiries(id) ON DELETE CASCADE,
    -- Until when the price stands; null on a job's quote, which does not lapse.
    ADD COLUMN valid_until timestamptz,
    -- A fixed price is a promise; an estimate says it may move, and that
    -- moving it is another decision.
    ADD COLUMN price_kind  text CHECK (price_kind IN ('fixed', 'estimate')),
    -- What a diagnosis may and may not find before the customer is asked again.
    ADD COLUMN limits      text,
    ADD CONSTRAINT quotes_one_subject CHECK ((work_order_id IS NULL) <> (enquiry_id IS NULL)),
    ADD CONSTRAINT quotes_enquiry_terms CHECK (enquiry_id IS NULL OR (valid_until IS NOT NULL AND price_kind IS NOT NULL)),
    ADD CONSTRAINT quotes_enquiry_version UNIQUE (enquiry_id, version);

ALTER TABLE quote_decisions
    ALTER COLUMN work_order_id DROP NOT NULL,
    ADD COLUMN enquiry_id uuid REFERENCES enquiries(id) ON DELETE CASCADE,
    -- The lines of the version the customer turned down while saying yes to
    -- the rest; null for a whole answer. The amount agreed is what is left.
    ADD COLUMN declined_positions integer[],
    ADD CONSTRAINT quote_decisions_one_subject CHECK ((work_order_id IS NULL) <> (enquiry_id IS NULL));

-- The accepted version travels with the booking made from it, and from there
-- to the job.
ALTER TABLE bookings
    ADD COLUMN enquiry_id uuid REFERENCES enquiries(id) ON DELETE RESTRICT,
    ADD COLUMN quote_id   uuid REFERENCES quotes(id)    ON DELETE RESTRICT;
CREATE INDEX bookings_enquiry_idx ON bookings (enquiry_id) WHERE enquiry_id IS NOT NULL;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['enquiries', 'enquiry_lines'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
    END LOOP;
END
$$;
-- UPDATE is for FOR UPDATE: the row is locked so that two people putting a
-- proposal forward, or two bookings, are taken one after the other. Nothing
-- writes to an enquiry's own columns after it is made.
GRANT SELECT, INSERT, UPDATE ON enquiries TO freesms_app;
GRANT SELECT, INSERT, UPDATE ON enquiry_lines TO freesms_app;
