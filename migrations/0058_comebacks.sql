-- rollback: safe
-- New tables the previous release never reads. Nothing it relies on is changed.
--
-- A car that comes back is not yet a warranty case. The customer says it is
-- not right; whether the first repair was at fault, whether a part failed,
-- whether it is something new that happens to be on the same car, is found out
-- afterwards and sometimes never. So opening a comeback records what was
-- reported and nothing about whose fault it is, and every opinion about that
-- is a row of its own, by a person, with their reason. The classification now
-- is the latest of them.
CREATE TABLE comebacks (
    id                     uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id                uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- The repair the customer says is not right, and when there was one
    -- particular operation on it, that line.
    original_work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE RESTRICT,
    original_line_id       uuid        REFERENCES work_order_lines(id) ON DELETE RESTRICT,
    -- The job the shop opened to look at it. One comeback, one job; a car that
    -- comes back twice for the same repair is two comebacks.
    follow_up_work_order_id uuid       NOT NULL UNIQUE REFERENCES work_orders(id) ON DELETE RESTRICT,
    symptoms               text        NOT NULL CHECK (length(trim(symptoms)) BETWEEN 1 AND 1000),
    -- The day the customer came back or rang, which is not always today.
    returned_on            date        NOT NULL,
    odometer_km            bigint      CHECK (odometer_km IS NULL OR (odometer_km >= 0 AND odometer_km < 10000000)),
    -- The customer on the follow-up is not the one on the original: the car
    -- was sold in between. The shop's obligation does not follow the person
    -- automatically, so it is shown rather than hidden.
    owner_changed          boolean     NOT NULL,
    opened_by              uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    opened_at              timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX comebacks_original_idx ON comebacks (original_work_order_id);

-- Money claimed back from a supplier for a part that failed. Status moves;
-- each move is also an event, so the history of a claim is not lost when it
-- is rejected and then argued.
CREATE TABLE comeback_claims (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    comeback_id  uuid        NOT NULL REFERENCES comebacks(id) ON DELETE RESTRICT,
    supplier     text        NOT NULL CHECK (length(trim(supplier)) BETWEEN 1 AND 100),
    reference    text        CHECK (reference IS NULL OR length(reference) <= 100),
    amount_minor bigint      CHECK (amount_minor IS NULL OR amount_minor >= 0),
    status       text        NOT NULL DEFAULT 'filed' CHECK (status IN ('filed', 'accepted', 'rejected', 'paid')),
    updated_at   timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX comeback_claims_comeback_idx ON comeback_claims (comeback_id);

CREATE TABLE comeback_events (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    comeback_id uuid        NOT NULL REFERENCES comebacks(id) ON DELETE RESTRICT,
    -- reported .. unrelated are the classification; claim_* are a claim's
    -- progress; note is something a person wanted written down.
    kind        text        NOT NULL CHECK (kind IN (
                                'reported', 'investigating', 'warranty', 'goodwill', 'unrelated',
                                'claim_filed', 'claim_accepted', 'claim_rejected', 'claim_paid', 'note')),
    note        text        CHECK (note IS NULL OR length(note) <= 500),
    claim_id    uuid        REFERENCES comeback_claims(id) ON DELETE RESTRICT,
    by_user     uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((kind LIKE 'claim\_%') = (claim_id IS NOT NULL)),
    -- A verdict is a statement by somebody, and says why.
    CHECK (kind NOT IN ('warranty', 'goodwill', 'unrelated', 'note') OR length(trim(coalesce(note, ''))) > 0)
);
CREATE INDEX comeback_events_comeback_idx ON comeback_events (comeback_id, at);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['comebacks', 'comeback_claims', 'comeback_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END $$;
GRANT UPDATE (reference, amount_minor, status, updated_at) ON comeback_claims TO freesms_app;
