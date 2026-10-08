-- rollback: safe
-- Triggers that announce a change and change nothing. The previous release
-- has nobody listening, and a notification nobody listens for is dropped.
--
-- Every screen used to show what was true when it was loaded. A committed
-- change now says, on one channel, which shop it was in and what it touched:
--
--   <shop id> job:<work order id>   the job page, and the lists it is on
--   <shop id> board                 the front desk's board, the technician's list
--   <shop id> parts                 the parts desk, the shelf
--   <shop id> calendar              the planner
--
-- Never a value. The screen asks again through its ordinary, role-checked
-- read; a technician's stream cannot carry a customer's name because nothing
-- on it is a name.
--
-- Postgres delivers a notification at commit, and only once per transaction
-- for the same channel and payload. A stock count that writes two hundred
-- rows is one "parts", not two hundred.

CREATE FUNCTION notify_change() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    r     jsonb;
    topic text;
    job   text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        r := to_jsonb(OLD);
    ELSE
        r := to_jsonb(NEW);
    END IF;

    FOREACH topic IN ARRAY TG_ARGV LOOP
        IF topic <> 'job' THEN
            PERFORM pg_notify('freesms_changes', (r->>'shop_id') || ' ' || topic);
            CONTINUE;
        END IF;

        -- Which job, directly or one step away. Read under the same row
        -- level security as the write, so it can only find this shop's.
        job := CASE WHEN TG_TABLE_NAME = 'work_orders' THEN r->>'id' ELSE r->>'work_order_id' END;
        IF job IS NULL AND r ? 'inspection_id' THEN
            SELECT work_order_id::text INTO job FROM inspections WHERE id = (r->>'inspection_id')::uuid;
        ELSIF job IS NULL AND TG_TABLE_NAME = 'inspection_decisions' THEN
            SELECT i.work_order_id::text INTO job
            FROM inspection_items it JOIN inspections i ON i.id = it.inspection_id
            WHERE it.id = (r->>'item_id')::uuid;
        ELSIF job IS NULL AND r ? 'invoice_id' THEN
            SELECT work_order_id::text INTO job FROM invoices WHERE id = (r->>'invoice_id')::uuid;
        ELSIF job IS NULL AND r ? 'request_id' THEN
            SELECT work_order_id::text INTO job FROM part_requests WHERE id = (r->>'request_id')::uuid;
        END IF;
        IF job IS NOT NULL THEN
            PERFORM pg_notify('freesms_changes', (r->>'shop_id') || ' job:' || job);
        END IF;
    END LOOP;
    RETURN NULL;
END
$$;

-- The job, and the lists a job is on.
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON work_orders
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON work_order_lines
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON findings
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON time_entries
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON vehicle_presence
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON customer_contacts
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON invoices
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON invoice_payments
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON inspections
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON inspection_items
    FOR EACH ROW EXECUTE FUNCTION notify_change('job');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON inspection_decisions
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON attachments
    FOR EACH ROW EXECUTE FUNCTION notify_change('job');

-- Parts: the desk's queue and the shelf, and the job they are for.
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON part_requests
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board', 'parts');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON part_request_receipts
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board', 'parts');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON stock_movements
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'parts');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON parts
    FOR EACH ROW EXECUTE FUNCTION notify_change('parts');

-- The planner.
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON bookings
    FOR EACH ROW EXECUTE FUNCTION notify_change('calendar');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON staff_absences
    FOR EACH ROW EXECUTE FUNCTION notify_change('calendar');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON shop_closures
    FOR EACH ROW EXECUTE FUNCTION notify_change('calendar');
