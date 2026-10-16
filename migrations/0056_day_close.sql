-- rollback: safe
-- New tables the previous release never reads.
--
-- The end of the day is a question the books have to be able to answer: what
-- was invoiced, what was credited, what came in and what went out, and does it
-- agree with what the bank, the card terminal and the till say. FreeSMS does
-- not hold the bank's figures and never assumes them; it records what a person
-- compared and what they made of any difference. And a day that was signed off
-- can still be corrected by the ledgers underneath it -- a payment entered a
-- week late, a credit dated yesterday -- so a sign-off remembers what the day
-- looked like when it was given, and the day says so when it no longer does.

-- What the other side said a method came to on a day, as typed by a person.
-- Appended to, never changed: the latest per day and method is the current
-- comparison, and the ones before it are how it got there.
CREATE TABLE day_settlements (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    day           date        NOT NULL,
    method        text        NOT NULL CHECK (method IN ('bank', 'cash', 'card', 'swish', 'other')),
    -- The total the bank statement, the terminal report or the till count gave.
    external_minor bigint     NOT NULL,
    -- What FreeSMS had recorded for the method that day when it was compared.
    recorded_minor bigint     NOT NULL,
    -- Why the two differ, required when they do.
    explanation   text        CHECK (explanation IS NULL OR length(explanation) <= 500),
    by_user       uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (external_minor = recorded_minor OR (explanation IS NOT NULL AND length(trim(explanation)) > 0))
);
CREATE INDEX day_settlements_day_idx ON day_settlements (day, method, at);

-- Somebody looked at the day and signed it off, and what it looked like then.
CREATE TABLE day_reviews (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    day           date        NOT NULL,
    version       integer     NOT NULL CHECK (version > 0),
    -- A hash of the day's documents and entries as they were, so a later
    -- change to either is recognised without keeping a copy of the books.
    fingerprint   text        NOT NULL,
    invoiced_gross_minor bigint NOT NULL,
    credited_gross_minor bigint NOT NULL,
    received_minor bigint     NOT NULL,
    paid_out_minor bigint     NOT NULL,
    open_exceptions integer   NOT NULL DEFAULT 0,
    note          text        CHECK (note IS NULL OR length(note) <= 500),
    by_user       uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (day, version)
);

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['day_settlements', 'day_reviews'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END $$;
