-- Core schema.
--
-- Principles applied throughout, so they are not repeated on every table:
--
--   * Every row that belongs to a shop carries shop_id, and every query
--     filters on it. Multi-tenancy added later is a rewrite.
--   * Money is bigint in minor units. No floats anywhere near an amount.
--   * VAT is basis points (2500 = 25%), so rates are integers too.
--   * Timestamps are timestamptz. The shop's local time is presentation.
--   * Anything that can happen more than once is a row, not a column.
--     Ownership, registrations, odometer readings and clocked time are all
--     events. They are appended, never overwritten.
--   * A CHECK constraint rather than a comment, wherever the valid values are
--     known.

-- btree_gist lets an exclusion constraint combine equality on a plain column
-- with overlap on a range. That is what stops two owners of one vehicle at the
-- same moment, and one registration on two vehicles at once.
CREATE EXTENSION IF NOT EXISTS btree_gist;


-- ─── Tenancy ────────────────────────────────────────────────────────────────

CREATE TABLE shops (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL CHECK (length(trim(name)) > 0),
    -- Presentation only. Stored times stay UTC.
    timezone   text        NOT NULL DEFAULT 'Europe/Stockholm',
    locale     text        NOT NULL DEFAULT 'en',
    currency   char(3)     NOT NULL DEFAULT 'SEK',
    created_at timestamptz NOT NULL DEFAULT now()
);


-- ─── People, access and billing parties ─────────────────────────────────────

-- A human being. One row per person, whatever their relationship to the shop.
--
-- A technician who has their own car serviced where they work is one person
-- with two relationships, not two records. Splitting them is how a system ends
-- up unable to answer who it is actually talking about.
--
-- Note what is absent: there is no national identity number column, and no
-- driving licence column. A field with no purpose does not exist. Adding one
-- "in case it is useful" is the exact pattern this project was started in
-- reaction to.
CREATE TABLE people (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    display_name text        NOT NULL CHECK (length(trim(display_name)) > 0),
    email        citext,
    phone        text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX people_shop_idx ON people (shop_id);
CREATE UNIQUE INDEX people_shop_email_idx ON people (shop_id, email) WHERE email IS NOT NULL;

-- Someone who can sign in. Staff only; customers do not get logins.
--
-- Roles are a constrained column rather than a table. Custom roles are not a
-- requirement, and four known values are easier to reason about -- and to
-- enforce -- than a table that can contain anything.
CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    person_id     uuid        NOT NULL REFERENCES people(id) ON DELETE RESTRICT,
    role          text        NOT NULL CHECK (role IN ('owner', 'service_advisor', 'technician', 'parts', 'admin')),
    password_hash text        NOT NULL,
    -- Deactivated, never deleted. A technician who leaves does not take their
    -- clocked hours and their work orders with them; those rows must stay
    -- attributable to a real person.
    active        boolean     NOT NULL DEFAULT true,
    deactivated_at timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK ((active AND deactivated_at IS NULL) OR (NOT active AND deactivated_at IS NOT NULL))
);
CREATE INDEX users_shop_idx ON users (shop_id);
CREATE UNIQUE INDEX users_person_idx ON users (person_id);

-- The party an invoice is addressed to: a private individual, or a company.
--
-- A company is not a person, which is why this is not simply a flag on people.
-- Reverse VAT for business customers in other EU countries needs the VAT
-- number, and that has nowhere sensible to live on a human being.
CREATE TABLE customers (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    kind        text        NOT NULL CHECK (kind IN ('private', 'company')),
    -- A private customer is a person; a company is not.
    person_id   uuid        REFERENCES people(id) ON DELETE RESTRICT,
    company_name text,
    org_number  text,
    vat_number  text,
    -- Invoice address. Nullable, because a workshop takes cars from people
    -- long before it needs to send anything by post.
    address_line1 text,
    address_line2 text,
    postal_code   text,
    city          text,
    country       char(2),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (kind = 'private' AND person_id IS NOT NULL AND company_name IS NULL) OR
        (kind = 'company' AND company_name IS NOT NULL)
    )
);
CREATE INDEX customers_shop_idx ON customers (shop_id);
CREATE INDEX customers_person_idx ON customers (person_id) WHERE person_id IS NOT NULL;


-- ─── Vehicles ───────────────────────────────────────────────────────────────

-- A vehicle is identified by its own id, not by anything printed on it.
--
-- VIN is closer to identity than a registration number but is still absent on
-- an unregistered import, a build, or an engine on a bench -- and is sometimes
-- simply wrong. A registration number is worse: personalised plates move
-- between cars, and deregistered numbers get reissued years later.
--
-- A vehicle with neither is allowed on purpose. A system that refuses to open
-- a job without an identifier gets worked around with junk records, and then
-- the history is genuinely lost rather than merely incomplete.
CREATE TABLE vehicles (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    vin         text        CHECK (vin IS NULL OR length(trim(vin)) > 0),
    make        text,
    model       text,
    model_year  smallint    CHECK (model_year IS NULL OR model_year BETWEEN 1885 AND 2100),
    engine      text,
    -- What to call it when there is no plate and no VIN: "the blue Amazon on
    -- lift 3", "engine, Karlsson".
    label       text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX vehicles_shop_idx ON vehicles (shop_id);
CREATE UNIQUE INDEX vehicles_shop_vin_idx ON vehicles (shop_id, vin) WHERE vin IS NOT NULL;

-- Registration numbers, as periods.
--
-- normalised strips spaces, hyphens and case so that "abc 12 d", "ABC12D" and
-- "ABC-12D" find the same vehicle. The original is kept for display, because
-- a plate is shown the way it is written.
CREATE TABLE vehicle_registrations (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    vehicle_id  uuid        NOT NULL REFERENCES vehicles(id) ON DELETE RESTRICT,
    registration text       NOT NULL CHECK (length(trim(registration)) > 0),
    normalised  text        NOT NULL,
    country     char(2),
    valid_from  timestamptz NOT NULL DEFAULT now(),
    valid_to    timestamptz,
    CHECK (valid_to IS NULL OR valid_to > valid_from),
    -- One plate cannot be on two vehicles at the same moment. It may be on a
    -- different vehicle later, which is exactly why this is an overlap
    -- constraint and not a unique index.
    EXCLUDE USING gist (
        shop_id WITH =,
        normalised WITH =,
        tstzrange(valid_from, valid_to) WITH &&
    )
);
CREATE INDEX vehicle_registrations_vehicle_idx ON vehicle_registrations (vehicle_id);
CREATE INDEX vehicle_registrations_lookup_idx ON vehicle_registrations (shop_id, normalised);

-- Ownership, as periods.
--
-- The technical history belongs to the vehicle and survives a sale. The
-- previous owner's identity does not travel with it: a new owner is shown what
-- was done to the car, never who owned it before or what they were charged.
-- Free text written by staff stays with the period it was written in, for the
-- same reason -- "customer refused the brake job" is not the next owner's
-- business.
CREATE TABLE vehicle_ownership (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    vehicle_id  uuid        NOT NULL REFERENCES vehicles(id) ON DELETE RESTRICT,
    customer_id uuid        NOT NULL REFERENCES customers(id) ON DELETE RESTRICT,
    owned_from  timestamptz NOT NULL DEFAULT now(),
    owned_to    timestamptz,
    CHECK (owned_to IS NULL OR owned_to > owned_from),
    EXCLUDE USING gist (
        vehicle_id WITH =,
        tstzrange(owned_from, owned_to) WITH &&
    )
);
CREATE INDEX vehicle_ownership_vehicle_idx ON vehicle_ownership (vehicle_id);
CREATE INDEX vehicle_ownership_customer_idx ON vehicle_ownership (customer_id);

-- Odometer readings, as events.
--
-- Always kilometres, and the column says so. Swedish "mil" is ten kilometres
-- and an English mile is 1.6, so a bare "mileage" column is a standing
-- invitation to store 1500 where 15000 was meant. Conversion is the user
-- interface's job, in one place.
--
-- A reading lower than the one before is recorded, not rejected: an instrument
-- cluster can legitimately be replaced, and a rolled-back odometer is
-- something the shop wants on record rather than something the software
-- refuses to hear about.
CREATE TABLE odometer_readings (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    vehicle_id  uuid        NOT NULL REFERENCES vehicles(id) ON DELETE RESTRICT,
    km          bigint      NOT NULL CHECK (km >= 0 AND km < 10000000),
    read_at     timestamptz NOT NULL DEFAULT now(),
    source      text        NOT NULL CHECK (source IN ('drop_off', 'collection', 'inspection', 'manual', 'import')),
    -- True when this reading is lower than the newest earlier one. Set by the
    -- application, kept so the anomaly survives in the record.
    decreasing  boolean     NOT NULL DEFAULT false,
    note        text,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX odometer_readings_vehicle_idx ON odometer_readings (vehicle_id, read_at DESC);


-- ─── Work ───────────────────────────────────────────────────────────────────

-- The work order points at the customer it was opened for, not at whoever owns
-- the vehicle today. A car sold next year must not change who last year's job
-- was for.
--
-- State transitions are enforced in one place in the application; the CHECK
-- here only guarantees the column never holds a value nobody planned for.
-- 'declined' does not mean "bill nothing": a customer who says no after the
-- car is in pieces still owes the diagnostic time.
CREATE TABLE work_orders (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    number       bigint      NOT NULL,
    vehicle_id   uuid        NOT NULL REFERENCES vehicles(id) ON DELETE RESTRICT,
    customer_id  uuid        NOT NULL REFERENCES customers(id) ON DELETE RESTRICT,
    state        text        NOT NULL DEFAULT 'draft' CHECK (state IN (
                                 'draft', 'estimated', 'awaiting_approval', 'approved',
                                 'in_progress', 'awaiting_parts', 'ready',
                                 'invoiced', 'closed', 'declined', 'cancelled')),
    complaint    text,
    opened_at    timestamptz NOT NULL DEFAULT now(),
    promised_at  timestamptz,
    closed_at    timestamptz,
    created_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (shop_id, number)
);
CREATE INDEX work_orders_shop_state_idx ON work_orders (shop_id, state);
CREATE INDEX work_orders_vehicle_idx ON work_orders (vehicle_id, opened_at DESC);
CREATE INDEX work_orders_customer_idx ON work_orders (customer_id);

-- One line of work or goods.
--
-- cost_bearer is what makes a warranty replacement expressible. The part still
-- appears on the order -- omitting it would destroy the record of what was
-- actually done to the car, which is the whole point of the history -- but at
-- zero, with the cost falling on the supplier or on the shop. All three look
-- identical to the customer and are completely different in the accounts, and
-- only 'supplier' is money that can be claimed back.
--
-- quantity is numeric because oil is not a count.
CREATE TABLE work_order_lines (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id         uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id   uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    position        integer     NOT NULL,
    kind            text        NOT NULL CHECK (kind IN ('labour', 'part', 'sublet', 'fee')),
    description     text        NOT NULL CHECK (length(trim(description)) > 0),
    quantity        numeric(12,3) NOT NULL CHECK (quantity >= 0),
    unit_price_minor bigint     NOT NULL,
    vat_rate_bp     integer     NOT NULL CHECK (vat_rate_bp BETWEEN 0 AND 10000),
    cost_bearer     text        NOT NULL DEFAULT 'customer'
                                CHECK (cost_bearer IN ('customer', 'supplier', 'shop')),
    -- A line added after the customer approved is not approved. It either goes
    -- back for approval or is marked on the invoice; it is never quietly
    -- included.
    approved_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- A warranty line is free to the customer by definition.
    CHECK (cost_bearer = 'customer' OR unit_price_minor = 0),
    UNIQUE (work_order_id, position)
);
CREATE INDEX work_order_lines_order_idx ON work_order_lines (work_order_id);

-- Clocked time.
--
-- Against a line where there is one, so labour billing and payroll come from
-- the same record rather than being reconstructed on Friday.
--
-- ended_at is nullable because a running clock is a real state. The overnight
-- clock-in -- a technician who forgets to stop and whose entry reads sixteen
-- hours -- is flagged by the application and left in the record; silently
-- truncating it hides a payroll dispute rather than settling one.
--
-- Rework under warranty is clocked like anything else and billed to the shop
-- or the supplier. Not recording it makes the technician who does the right
-- thing look unproductive for doing it.
CREATE TABLE time_entries (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE RESTRICT,
    line_id       uuid        REFERENCES work_order_lines(id) ON DELETE SET NULL,
    user_id       uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    started_at    timestamptz NOT NULL,
    ended_at      timestamptz,
    -- Set when a supervisor corrects an entry. The original values stay in
    -- corrected_from_* so the correction is visible rather than invisible.
    corrected_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    corrected_at  timestamptz,
    corrected_from_started_at timestamptz,
    corrected_from_ended_at   timestamptz,
    flagged       boolean     NOT NULL DEFAULT false,
    note          text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (ended_at IS NULL OR ended_at > started_at)
);
CREATE INDEX time_entries_order_idx ON time_entries (work_order_id);
CREATE INDEX time_entries_user_idx ON time_entries (user_id, started_at DESC);
-- One technician, one running clock. Two open entries for the same person is
-- ambiguity that becomes a payroll dispute later.
CREATE UNIQUE INDEX time_entries_one_running_per_user_idx
    ON time_entries (user_id) WHERE ended_at IS NULL;


-- ─── Parts and attachments ──────────────────────────────────────────────────

-- The catalogue. Stock levels are deliberately not a column here: quantity on
-- hand is the sum of movements, added in the inventory work, because a written
-- over number cannot answer what a year of wrongly ordered parts cost.
CREATE TABLE parts (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    number      text        NOT NULL CHECK (length(trim(number)) > 0),
    name        text        NOT NULL CHECK (length(trim(name)) > 0),
    -- Fluids and consumables are sold by volume, so a unit is not always
    -- "each".
    unit        text        NOT NULL DEFAULT 'each'
                            CHECK (unit IN ('each', 'litre', 'metre', 'kilogram', 'hour')),
    cost_minor  bigint,
    price_minor bigint,
    vat_rate_bp integer     CHECK (vat_rate_bp IS NULL OR vat_rate_bp BETWEEN 0 AND 10000),
    active      boolean     NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (shop_id, number)
);
CREATE INDEX parts_shop_idx ON parts (shop_id) WHERE active;

-- Photographs and documents.
--
-- The file itself lives on disk under ATTACHMENTS_DIR, not in the database.
-- It is the only thing in this system that cannot be regenerated from a dump,
-- which is why it is called out separately in DEPLOY.md.
--
-- An inspection photograph taken in a workshop catches things it was not aimed
-- at -- another customer's plate, a colleague, paperwork on a bench -- so an
-- attachment is personal data and deleting one has to delete the file too.
CREATE TABLE attachments (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        REFERENCES work_orders(id) ON DELETE RESTRICT,
    vehicle_id    uuid        REFERENCES vehicles(id) ON DELETE RESTRICT,
    storage_key   text        NOT NULL UNIQUE,
    content_type  text        NOT NULL,
    byte_size     bigint      NOT NULL CHECK (byte_size > 0),
    original_name text,
    uploaded_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (work_order_id IS NOT NULL OR vehicle_id IS NOT NULL)
);
CREATE INDEX attachments_order_idx ON attachments (work_order_id) WHERE work_order_id IS NOT NULL;
CREATE INDEX attachments_vehicle_idx ON attachments (vehicle_id) WHERE vehicle_id IS NOT NULL;
