-- The shop's own labour times.
--
-- Licensed guides -- MOTOR, ALLDATA, Mitchell -- cannot ship with an open
-- source project. That looks like the fatal gap until you read what shops say
-- about the paid ones: "labor guide is almost always wrong", and a system
-- charging over 200 USD a month with no times for common jobs at all.
--
-- Small shops already keep their own times, on paper or in somebody's head.
-- This makes that a feature rather than a workaround.

CREATE TABLE labour_times (
    id        uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id   uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    operation text    NOT NULL CHECK (length(trim(operation)) > 0),

    -- Scope. NULL is a wildcard: a time for "Volvo, any model" has make set
    -- and the rest NULL.
    --
    -- A time that is right for one engine variant is wrong for the next, so
    -- the scope has to be expressible at that grain -- and the matching has to
    -- say which rule it used, because a suggestion nobody can account for is
    -- one nobody trusts.
    make      text,
    model     text,
    year_from smallint,
    year_to   smallint,
    engine    text,

    -- The book time, in minutes. Integers: a tenth of an hour is six minutes
    -- and nobody needs finer.
    minutes   integer NOT NULL CHECK (minutes > 0 AND minutes <= 24 * 60),

    -- Kept apart from the book time on purpose. A rusted bolt is not the book
    -- time, and a shop that adds fifteen minutes for its own conditions should
    -- be able to see that it has done so rather than losing it inside a number
    -- it can no longer explain.
    adjustment_minutes integer NOT NULL DEFAULT 0
        CHECK (adjustment_minutes > -24 * 60 AND adjustment_minutes < 24 * 60),

    note      text,
    active    boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    CHECK (year_from IS NULL OR year_to IS NULL OR year_to >= year_from),
    -- A model without a make is not a scope anybody can reason about.
    CHECK (make IS NOT NULL OR model IS NULL),
    CHECK (minutes + adjustment_minutes > 0)
);
CREATE INDEX labour_times_lookup_idx ON labour_times (shop_id, operation) WHERE active;

ALTER TABLE labour_times ENABLE ROW LEVEL SECURITY;
ALTER TABLE labour_times FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON labour_times
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- Which stored time a line came from, so that what was actually clocked can be
-- compared with what was expected.
--
-- Without this the library never learns, and a wildly wrong entry poisons
-- every future estimate quietly. With it the shop can see the spread of real
-- times next to the stored one and decide for itself.
ALTER TABLE work_order_lines ADD COLUMN labour_time_id uuid
    REFERENCES labour_times(id) ON DELETE SET NULL;
CREATE INDEX work_order_lines_labour_time_idx ON work_order_lines (labour_time_id)
    WHERE labour_time_id IS NOT NULL;

GRANT SELECT, INSERT, UPDATE, DELETE ON labour_times TO redasms_app;
