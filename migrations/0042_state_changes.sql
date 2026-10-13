-- rollback: safe
-- A new table and a trigger that only adds to it. The previous release never
-- reads the table, and its state changes are recorded the same.
--
-- Every change of a work order's state, as a row. The state column says where
-- a job is; it cannot say that it was ready on Tuesday and went back on a
-- lift on Wednesday, and a final check done on Monday says nothing about
-- Wednesday's work. Written by a trigger, so no path that moves a job -- the
-- state machine, invoicing, closing, a correction by hand -- can forget.
--
-- There is no history before this migration: jobs moved before it have
-- their first row at their next move.

CREATE TABLE work_order_state_changes (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    -- Null when the order was created.
    from_state    text,
    to_state      text        NOT NULL,
    -- Who, where a signed-in person did it; null for the system and for a
    -- change made outside the application.
    changed_by    uuid        REFERENCES users(id) ON DELETE RESTRICT,
    at            timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX work_order_state_changes_order_idx ON work_order_state_changes (work_order_id, at);

ALTER TABLE work_order_state_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE work_order_state_changes FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON work_order_state_changes
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON work_order_state_changes FROM freesms_app;
GRANT SELECT, INSERT ON work_order_state_changes TO freesms_app;

CREATE FUNCTION record_state_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO work_order_state_changes (shop_id, work_order_id, from_state, to_state, changed_by)
    VALUES (NEW.shop_id, NEW.id, CASE WHEN TG_OP = 'UPDATE' THEN OLD.state END, NEW.state,
            nullif(current_setting('app.current_user', true), '')::uuid);
    RETURN NULL;
END
$$;

CREATE TRIGGER record_state_change AFTER INSERT ON work_orders
    FOR EACH ROW EXECUTE FUNCTION record_state_change();
CREATE TRIGGER record_state_change_update AFTER UPDATE OF state ON work_orders
    FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state) EXECUTE FUNCTION record_state_change();
