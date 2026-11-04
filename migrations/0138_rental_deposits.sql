-- rollback: safe
-- New tables, and a nullable column on damage_case_steps the previous
-- release neither writes nor reads.
--
-- A rental's deposit (#196). The money in it -- received, given back,
-- applied to a claim -- is the payment journal's (0078): a deposit of
-- purpose 'rental' against the booking, its rows in invoice_payments. What
-- the journal cannot hold is what moved no money: the amount asked for and
-- how it is to be paid, and a hold on the customer's card taken in the
-- shop's own terminal, with until when it lasts. Those are events here.
-- None is a counter: what is asked, held, received, given back and applied
-- is read from the rows, never kept.
CREATE TABLE rental_deposits (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    booking_id  uuid        NOT NULL UNIQUE REFERENCES rental_bookings(id) ON DELETE RESTRICT,
    -- The journal's deposit; its party is who pays it, the renter or a
    -- company paying for them.
    deposit_id  uuid        NOT NULL UNIQUE REFERENCES deposits(id) ON DELETE RESTRICT,
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by  uuid        REFERENCES users(id) ON DELETE RESTRICT
);

CREATE TABLE rental_deposit_events (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    rental_deposit_id uuid   NOT NULL REFERENCES rental_deposits(id) ON DELETE RESTRICT,
    seq          bigint      GENERATED ALWAYS AS IDENTITY,
    -- required: what is asked and how it is to come; held: a hold on the
    -- customer's card, no money moved; hold_released: the hold let go.
    kind         text        NOT NULL CHECK (kind IN ('required', 'held', 'hold_released')),
    amount_minor bigint      CHECK (amount_minor IS NULL OR amount_minor BETWEEN 0 AND 10000000000),
    method       text        CHECK (method IS NULL OR method IN ('card_hold', 'cash', 'card', 'bank', 'swish', 'other')),
    -- The terminal's reference for the hold.
    reference    text        CHECK (reference IS NULL OR length(btrim(reference)) BETWEEN 1 AND 100),
    -- Until when the card issuer keeps the hold.
    valid_until  timestamptz,
    reason       text        CHECK (reason IS NULL OR length(btrim(reason)) BETWEEN 1 AND 300),
    at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user      uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (kind <> 'required' OR (amount_minor IS NOT NULL AND method IS NOT NULL)),
    CHECK (kind <> 'held' OR (amount_minor > 0 AND reference IS NOT NULL AND valid_until IS NOT NULL)),
    CHECK (kind <> 'hold_released' OR (amount_minor IS NULL AND reason IS NOT NULL))
);
CREATE INDEX rental_deposit_events_idx ON rental_deposit_events (rental_deposit_id, seq);

-- Which damage case each application of a rental deposit settles, so what
-- is taken for a case is never more than was settled for it.
CREATE TABLE rental_deposit_applications (
    shop_id            uuid NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    deposit_payment_id uuid NOT NULL PRIMARY KEY REFERENCES invoice_payments(id) ON DELETE RESTRICT,
    damage_case_id     uuid NOT NULL REFERENCES damage_cases(id) ON DELETE RESTRICT
);
CREATE INDEX rental_deposit_applications_case_idx ON rental_deposit_applications (damage_case_id);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['rental_deposits', 'rental_deposit_events', 'rental_deposit_applications'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END;
$$;
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON rental_deposit_events
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');

-- By when the customer may object, said when they are told (#196): a
-- deposit is applied to an undisputed claim only once that has passed.
ALTER TABLE damage_case_steps ADD COLUMN respond_by date;
