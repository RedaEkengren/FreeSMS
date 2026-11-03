-- rollback: safe
-- New tables, one more kind of odometer reading and one more subject an
-- audit event may have, which the previous release never writes. Nothing
-- in the previous release reads them.
--
-- A test drive (#173): a car in stock held for a slot on the availability
-- ledger (resource_blocks, 0070) -- against the workshop, rentals,
-- deliveries and other drives -- with the driver, the lead it came from,
-- the licence checked and the agreement recorded before the keys leave,
-- and the time and the odometer when it leaves and comes back.
CREATE TABLE test_drives (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id         uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    stock_unit_id   uuid        NOT NULL REFERENCES stock_units(id) ON DELETE RESTRICT,
    driver_party_id uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    lead_id         uuid        REFERENCES leads(id) ON DELETE RESTRICT,
    -- The slot on the ledger; what it says is the booking.
    block_id        uuid        NOT NULL UNIQUE REFERENCES resource_blocks(id) ON DELETE RESTRICT,
    starts_at       timestamptz NOT NULL,
    ends_at         timestamptz NOT NULL CHECK (ends_at > starts_at),
    -- Where the driver may go, as the agreement says it.
    area            text        CHECK (area IS NULL OR length(area) <= 300),
    agreement_id    uuid        REFERENCES document_records(id) ON DELETE RESTRICT,
    request_key     text        CHECK (request_key IS NULL OR length(request_key) <= 100),
    booked_at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    booked_by       uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX test_drives_request_idx ON test_drives (shop_id, request_key) WHERE request_key IS NOT NULL;
CREATE INDEX test_drives_unit_idx ON test_drives (stock_unit_id, starts_at);
CREATE INDEX test_drives_lead_idx ON test_drives (lead_id) WHERE lead_id IS NOT NULL;

-- The driver's licence, checked: only that it was looked at, by whom and
-- when, what it allows and until when. No licence or identity number is
-- kept here; one that must be kept goes to identity_records (0084) with
-- its purpose.
CREATE TABLE test_drive_licence_checks (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id         uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    test_drive_id   uuid        NOT NULL REFERENCES test_drives(id) ON DELETE RESTRICT,
    id_checked      boolean     NOT NULL,
    method          text        NOT NULL CHECK (method IN ('card_inspected', 'digital_licence', 'foreign_inspected', 'other')),
    categories      text[]      NOT NULL DEFAULT '{}'
        CHECK (cardinality(categories) <= 12 AND array_to_string(categories, ',') ~ '^([A-Z0-9]{1,4}(,[A-Z0-9]{1,4})*)?$'),
    valid_until     date,
    -- The driver is at least the shop's minimum age for a test drive.
    age_ok          boolean     NOT NULL,
    note            text        CHECK (note IS NULL OR length(note) <= 300),
    checked_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    checked_by      uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX test_drive_licence_checks_drive_idx ON test_drive_licence_checks (test_drive_id, checked_at DESC);

-- What happened to a drive, in order: started (the keys handed over),
-- returned, or cancelled before it started. Each once.
CREATE TABLE test_drive_events (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id             uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    test_drive_id       uuid        NOT NULL REFERENCES test_drives(id) ON DELETE RESTRICT,
    kind                text        NOT NULL CHECK (kind IN ('started', 'returned', 'cancelled')),
    at                  timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user             uuid        REFERENCES users(id) ON DELETE RESTRICT,
    odometer_reading_id uuid        REFERENCES odometer_readings(id) ON DELETE RESTRICT,
    -- Why a drive started without a signed agreement, why it was
    -- cancelled, or what was seen on return: observed, never a debt.
    note                text        CHECK (note IS NULL OR length(note) <= 500),
    UNIQUE (test_drive_id, kind),
    CHECK (kind = 'cancelled' OR odometer_reading_id IS NOT NULL)
);

-- How the shop runs its test drives: the youngest driver it lets out, and
-- whether a car reserved for somebody else may be driven by another (a
-- warning) or not at all. A setting changed is a row.
CREATE TABLE test_drive_settings (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id             uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    seq                 bigint      GENERATED ALWAYS AS IDENTITY,
    minimum_age         integer     NOT NULL CHECK (minimum_age BETWEEN 16 AND 99),
    reserved_blocks     boolean     NOT NULL,
    changed_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by          uuid        REFERENCES users(id) ON DELETE RESTRICT
);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['test_drives', 'test_drive_licence_checks', 'test_drive_events', 'test_drive_settings'] LOOP
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
-- The agreement is produced after the drive is booked.
GRANT UPDATE (agreement_id) ON test_drives TO freesms_app;

-- Odometer readings taken as a drive starts and as it comes back.
SELECT widen_text_check('odometer_readings', 'odometer_readings_source_check', 'source', ARRAY['test_drive']);
-- A drive started without a signed agreement is audited on the drive.
SELECT widen_text_check('audit_events', 'audit_events_subject_kind_check', 'subject_kind', ARRAY['test_drive']);
