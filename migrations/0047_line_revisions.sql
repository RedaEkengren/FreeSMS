-- rollback: restore
-- A line on a job could be added and never corrected. A misspelt description,
-- a quantity of 10 where 1 was meant, a part that turned out to be the wrong
-- one: the only remedy was a second line that cancelled the first, and the
-- history then said something the shop did not mean.
--
-- A line is now a row with a history of its own. work_order_lines keeps the
-- current values, so everything that reads a line keeps working; this table
-- keeps every version that has ever been the current one, with who and when,
-- because a number written over cannot answer "what did it say when the
-- customer agreed". What the customer agreed to is frozen separately, in
-- quote_lines; this is the shop's own trail.
--
-- Restore, not safe: a line withdrawn here is excluded from the invoice by
-- code that exists only in this release. The previous release would read the
-- same row, see nothing wrong with it and invoice it.

ALTER TABLE work_order_lines
    -- Counts the versions. A form carries the one it was drawn from, so an
    -- edit made against a line somebody else has since changed is refused
    -- instead of silently overwriting theirs.
    ADD COLUMN revision         integer     NOT NULL DEFAULT 1 CHECK (revision > 0),
    -- Withdrawn: entered by mistake. Unlike declined_at, which is the
    -- customer saying no to something offered, this is the shop saying the
    -- line should never have been there. The row stays; nothing counts it.
    ADD COLUMN withdrawn_at     timestamptz,
    ADD COLUMN withdrawn_by     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    ADD COLUMN withdrawn_reason text        CHECK (withdrawn_reason IS NULL OR length(trim(withdrawn_reason)) BETWEEN 1 AND 300);

ALTER TABLE work_order_lines ADD CONSTRAINT work_order_lines_withdrawn_whole
    CHECK ((withdrawn_at IS NULL) = (withdrawn_by IS NULL)
       AND (withdrawn_at IS NULL) = (withdrawn_reason IS NULL));

-- Work that really happened is not withdrawn: it is corrected, or it stays.
ALTER TABLE work_order_lines ADD CONSTRAINT work_order_lines_withdrawn_not_performed
    CHECK (withdrawn_at IS NULL OR performed_at IS NULL);

CREATE TABLE work_order_line_revisions (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id          uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    line_id          uuid        NOT NULL REFERENCES work_order_lines(id) ON DELETE CASCADE,
    revision         integer     NOT NULL CHECK (revision > 0),
    change           text        NOT NULL CHECK (change IN ('added', 'edited', 'withdrawn')),
    kind             text        NOT NULL,
    description      text        NOT NULL,
    quantity         numeric(12,3) NOT NULL,
    unit_price_minor bigint      NOT NULL,
    vat_rate_bp      integer     NOT NULL,
    cost_bearer      text        NOT NULL,
    part_id          uuid        REFERENCES parts(id) ON DELETE SET NULL,
    -- Why: required for a withdrawal, optional for an edit.
    reason           text        CHECK (reason IS NULL OR length(reason) <= 300),
    -- Whether the edit took the customer's agreement off the line.
    approval_cleared boolean     NOT NULL DEFAULT false,
    -- Null for the first version of a line that existed before this table,
    -- and for one written by a trigger, which cannot know who was typing.
    by_user          uuid        REFERENCES users(id) ON DELETE RESTRICT,
    at               timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (line_id, revision)
);

-- Every line that exists gets its present state as version 1.
INSERT INTO work_order_line_revisions
    (shop_id, line_id, revision, change, kind, description, quantity,
     unit_price_minor, vat_rate_bp, cost_bearer, part_id, at)
SELECT shop_id, id, 1, 'added', kind, description, quantity,
       unit_price_minor, vat_rate_bp, cost_bearer, part_id, created_at
FROM work_order_lines;

-- And so does every line from now on, wherever it is written from (typing,
-- the shelf, an accepted enquiry), so no caller can forget.
CREATE FUNCTION first_line_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO work_order_line_revisions
        (shop_id, line_id, revision, change, kind, description, quantity,
         unit_price_minor, vat_rate_bp, cost_bearer, part_id, at)
    VALUES (NEW.shop_id, NEW.id, 1, 'added', NEW.kind, NEW.description, NEW.quantity,
            NEW.unit_price_minor, NEW.vat_rate_bp, NEW.cost_bearer, NEW.part_id, NEW.created_at);
    RETURN NULL;
END
$$;
CREATE TRIGGER first_line_revision AFTER INSERT ON work_order_lines
    FOR EACH ROW EXECUTE FUNCTION first_line_revision();

ALTER TABLE work_order_line_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_order_line_revisions FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON work_order_line_revisions
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON work_order_line_revisions FROM freesms_app;
GRANT SELECT, INSERT ON work_order_line_revisions TO freesms_app;

-- One line per approved inspection item stays true of the lines that count. A
-- withdrawn one no longer holds the item, so it can be priced again.
DROP INDEX work_order_lines_one_per_item;
CREATE UNIQUE INDEX work_order_lines_one_per_item
    ON work_order_lines (inspection_item_id)
    WHERE inspection_item_id IS NOT NULL AND withdrawn_at IS NULL;
