-- rollback: safe
-- New tables, and one more subject an audit event may have, which the
-- previous release never writes. The previous release reads none of it.
--
-- It also changes Core's payouts table (0078): a CHECK that a payout names
-- only what its purpose is for, after taking the subject off any payout
-- the previous release wrote that names something else (see the end of
-- this file). The previous release reads payouts and still runs: it never
-- asks for the constraint, and the payouts it writes from its pages name
-- no subject.
--
-- Before money leaves for a car, the buyer has checked, and recorded, that
-- it is not under a driving ban, carries no credit purchase or undisclosed
-- lender, owes no vehicle tax Kronofogden could pursue, has a known
-- inspection status, has its lessor's consent if it is leased, carries the
-- chassis number its registration says, and shows an odometer reading that
-- fits what was read before (#162). Each check is a row; checking again is
-- another row, and the newest of each kind is what stands. A trade-in and a
-- consigned car are checked the same way, so the checks belong to the stock
-- unit, not to a purchase.
CREATE TABLE stock_checks (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id              uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    stock_unit_id        uuid        NOT NULL REFERENCES stock_units(id) ON DELETE RESTRICT,
    -- Its order among the unit's checks, whatever the clock says.
    seq                  bigint      GENERATED ALWAYS AS IDENTITY,
    kind                 text        NOT NULL CHECK (kind IN ('driving_ban', 'credit_purchase', 'unpaid_tax', 'inspection',
                                                              'leasing_consent', 'vin_matches', 'odometer')),
    result               text        NOT NULL CHECK (result IN ('pass', 'fail', 'unknown')),
    -- Who said so: a person, a data provider through the vehicle lookup
    -- (#155), or the system comparing readings it holds.
    source               text        NOT NULL CHECK (source IN ('manual', 'provider', 'system')),
    provider             text        CHECK (provider IS NULL OR length(provider) <= 100),
    checked_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    checked_by           uuid        REFERENCES users(id) ON DELETE RESTRICT,
    evidence_document_id uuid        REFERENCES document_records(id) ON DELETE RESTRICT,
    note                 text        CHECK (note IS NULL OR length(note) <= 500),
    CHECK ((source = 'provider') = (provider IS NOT NULL))
);
CREATE INDEX stock_checks_unit_idx ON stock_checks (stock_unit_id, kind, seq DESC);

ALTER TABLE stock_checks ENABLE ROW LEVEL SECURITY;
ALTER TABLE stock_checks FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON stock_checks
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON stock_checks FROM freesms_app;
GRANT SELECT, INSERT ON stock_checks TO freesms_app;

-- Which checks the shop requires before money leaves, and how old a check
-- may be: a setting changed is a row, the newest of each kind standing.
-- With no row, a check is required and may be a day old.
CREATE TABLE stock_check_settings (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    seq        bigint      GENERATED ALWAYS AS IDENTITY,
    kind       text        NOT NULL CHECK (kind IN ('driving_ban', 'credit_purchase', 'unpaid_tax', 'inspection',
                                                    'leasing_consent', 'vin_matches', 'odometer')),
    required   boolean     NOT NULL,
    max_age    interval    NOT NULL CHECK (max_age > '0' AND max_age <= '30 days'),
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX stock_check_settings_kind_idx ON stock_check_settings (shop_id, kind, seq DESC);

ALTER TABLE stock_check_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE stock_check_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON stock_check_settings
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON stock_check_settings FROM freesms_app;
GRANT SELECT, INSERT ON stock_check_settings TO freesms_app;

-- A loan the check found on the car: to whom, how much, and the reference
-- to settle it with. What is paid to the lender is still part of the tax
-- purchase price (#161); the seller is paid the rest, or owes the
-- difference when the loan is the larger.
CREATE TABLE purchase_loans (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id              uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    purchase_id          uuid        NOT NULL REFERENCES vehicle_purchases(id) ON DELETE RESTRICT,
    lender_party_id      uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    amount_minor         bigint      NOT NULL CHECK (amount_minor > 0 AND amount_minor <= 100000000000),
    settlement_reference text        NOT NULL CHECK (length(btrim(settlement_reference)) BETWEEN 1 AND 100),
    recorded_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    recorded_by          uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (purchase_id, lender_party_id)
);

ALTER TABLE purchase_loans ENABLE ROW LEVEL SECURITY;
ALTER TABLE purchase_loans FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON purchase_loans
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON purchase_loans FROM freesms_app;
GRANT SELECT, INSERT ON purchase_loans TO freesms_app;

-- An owner letting money leave although a check stands failed, unknown or
-- too old: who, when and why, and which kinds of check kept it back then.
-- It covers those kinds alone, for as long as a check of each would stand
-- from the moment it was made, and not past a check of the kind made
-- after it -- that is new information.
CREATE TABLE stock_check_overrides (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    stock_unit_id uuid        NOT NULL REFERENCES stock_units(id) ON DELETE RESTRICT,
    -- The newest check of the unit it was made over.
    upto_seq      bigint      NOT NULL,
    -- The kinds of check outstanding when it was made: what it covers.
    kinds         text[]      NOT NULL CHECK (cardinality(kinds) > 0),
    by_user       uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    reason        text        NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 300)
);
CREATE INDEX stock_check_overrides_unit_idx ON stock_check_overrides (stock_unit_id, at DESC);

ALTER TABLE stock_check_overrides ENABLE ROW LEVEL SECURITY;
ALTER TABLE stock_check_overrides FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON stock_check_overrides
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON stock_check_overrides FROM freesms_app;
GRANT SELECT, INSERT ON stock_check_overrides TO freesms_app;

-- A payout names what it pays only for the purpose it is for: a purchase's
-- names a purchase, a consignment's a consignment, and one for anything
-- else names nothing. A payout for something else naming a car would let
-- money leave for that car without its checks, an override or a reason.
-- Refused here as well as in the application, whatever writes it. The
-- previous release has no page that names a payout's subject at all, but
-- its CreatePayout never refused one, so a payout for something else
-- naming a purchase or a consignment may exist, written through it by a
-- script or an integration.
--
-- Such a payout is not deleted or moved to another purpose: the money it
-- records left, for what the shop said, and neither is ours to change.
-- Its subject is taken off -- it never was what the payout was for, and
-- no check was asked of it -- and what it named is kept at the head of
-- its note, so nothing it said is lost. The constraint is added NOT VALID
-- first, so every payout written from here on is held to it while the
-- old ones are put right, and then validated over them all.
ALTER TABLE payouts ADD CONSTRAINT payouts_subject_is_its_purpose
    CHECK (subject_kind IS NULL OR subject_kind = purpose) NOT VALID;
UPDATE payouts
   SET note = left('[Named ' || subject_kind || ' ' || subject_id || ', which it was not for] ' || coalesce(note, ''), 300),
       subject_kind = NULL,
       subject_id = NULL
 WHERE subject_kind IS NOT NULL AND subject_kind <> purpose;
ALTER TABLE payouts VALIDATE CONSTRAINT payouts_subject_is_its_purpose;
