-- rollback: safe
-- New functions and triggers, and a function replaced with the same
-- signature. The previous release's reversal of a job's posting finds it
-- already reversed and does nothing; its other writes are as before.
--
-- One way to reverse a cost (#164), whoever asks: a person by hand
-- (ReverseStockCost), a supplier's corrected invoice, or a job that has
-- changed since its cost was posted. Each takes the cost's job before the
-- cost's own lock -- the order every path that reverses a cost takes -- and
-- a cost already reversed, or that is a reversal, is left as it is
-- (NULL). The reversal copies every column of what it takes back, from
-- where it came (a supplier's cost reversed is entered by hand) and for
-- the same moment, so on the same side of the sale.
CREATE FUNCTION stock_cost_reverse(entry uuid, on_day date, why text, by_user uuid) RETURNS uuid
LANGUAGE plpgsql AS $$
DECLARE
    reversal uuid;
BEGIN
    PERFORM 1 FROM work_orders WHERE id = (SELECT work_order_id FROM stock_cost_entries WHERE id = entry) FOR SHARE;
    PERFORM pg_advisory_xact_lock(hashtextextended('cost:' || entry::text, 0));
    INSERT INTO stock_cost_entries (shop_id, stock_unit_id, kind, net_minor, input_vat_minor, vat_rate_basis,
                                    supplier_party_id, external_ref, incurred_on, source, work_order_id, happened_at,
                                    currency, original_net_minor, rate_micro, rate_source, note, reverses_id, recorded_by)
    SELECT e.shop_id, e.stock_unit_id, e.kind, -e.net_minor, -e.input_vat_minor, e.vat_rate_basis, e.supplier_party_id,
           e.external_ref, on_day, CASE WHEN e.source = 'outsourced' THEN 'manual' ELSE e.source END, e.work_order_id,
           e.happened_at, e.currency, -e.original_net_minor, e.rate_micro, e.rate_source, why, e.id, by_user
      FROM stock_cost_entries e
     WHERE e.id = entry AND e.reverses_id IS NULL
       AND NOT EXISTS (SELECT 1 FROM stock_cost_entries r WHERE r.reverses_id = e.id)
    RETURNING id INTO reversal;
    RETURN reversal;
END;
$$;

-- A supplier's invoice noted again replaces the one before (0099): its
-- cost is reversed through the one way, then the new one posted.
CREATE OR REPLACE FUNCTION outsourced_invoice_costs_stock() RETURNS trigger AS $$
DECLARE
    unit uuid;
    happened timestamptz;
    day date;
BEGIN
    IF NEW.kind <> 'invoice' THEN
        RETURN NEW;
    END IF;
    day := (NEW.at AT TIME ZONE (SELECT timezone FROM shops WHERE id = NEW.shop_id))::date;
    PERFORM stock_cost_reverse(e.id, day, NEW.invoice_ref, NEW.by_user)
       FROM stock_cost_entries e
       JOIN outsourced_events oe ON oe.id = e.outsourced_event_id
      WHERE oe.outsourced_id = NEW.outsourced_id
      ORDER BY e.id;
    IF NEW.amount_minor IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT u.id, w.opened_at INTO unit, happened
      FROM outsourced_work o
      JOIN work_orders w ON w.id = o.work_order_id
      JOIN stock_units u ON u.vehicle_id = w.vehicle_id AND u.opened_at <= w.opened_at
                         AND (u.closed_at IS NULL OR u.closed_at > w.opened_at)
     WHERE o.id = NEW.outsourced_id;
    IF unit IS NULL THEN
        RETURN NEW;
    END IF;
    INSERT INTO stock_cost_entries (shop_id, stock_unit_id, kind, net_minor, input_vat_minor, incurred_on, source,
                                    work_order_id, outsourced_event_id, external_ref, happened_at, recorded_by)
    SELECT NEW.shop_id, unit, 'workshop', NEW.amount_minor, round(NEW.amount_minor * 0.25)::bigint, day, 'outsourced',
           o.work_order_id, NEW.id, NEW.invoice_ref, happened, NEW.by_user
      FROM outsourced_work o WHERE o.id = NEW.outsourced_id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- A job's posting at cost (PostJobCost) is what the job cost. Once anything
-- changes that -- the job back at work by whatever path (a task added or
-- reopened, a part requested, a person's move), a part taken out or put
-- back, its hours changed -- the posting no longer stands: it is reversed
-- with why, with an audit event on the car's stock unit, and the job posts
-- again once it is ready. In the database, so no path of any release can
-- leave a stale posting behind. A customer's job has no posting, and
-- nothing happens.
CREATE FUNCTION stock_cost_job_changed(job uuid, why text) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    e record;
    reversal uuid;
    actor uuid := nullif(current_setting('app.current_user', true), '')::uuid;
BEGIN
    FOR e IN SELECT c.id, c.shop_id, c.stock_unit_id, c.net_minor FROM stock_cost_entries c
              WHERE c.work_order_id = job AND c.source = 'work_order' AND c.reverses_id IS NULL
                AND NOT EXISTS (SELECT 1 FROM stock_cost_entries r WHERE r.reverses_id = c.id)
              ORDER BY c.id LOOP
        reversal := stock_cost_reverse(e.id, (clock_timestamp() AT TIME ZONE (SELECT timezone FROM shops WHERE id = e.shop_id))::date,
                                       why, actor);
        IF reversal IS NOT NULL THEN
            INSERT INTO audit_events (shop_id, subject_kind, subject_id, event, actor, reason, detail)
            VALUES (e.shop_id, 'stock_unit', e.stock_unit_id, 'stock_cost.reversed', actor, why,
                    jsonb_build_object('work_order_id', job, 'entry_id', e.id, 'reversal_id', reversal, 'net_minor', e.net_minor));
        END IF;
    END LOOP;
END;
$$;

CREATE FUNCTION stock_cost_job_back_to_work() RETURNS trigger AS $$
BEGIN
    PERFORM stock_cost_job_changed(NEW.id, 'The job went back to work after it was ready');
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER stock_cost_job_back_to_work AFTER UPDATE OF state ON work_orders
    FOR EACH ROW WHEN (OLD.state = 'ready' AND NEW.state = 'in_progress')
    EXECUTE FUNCTION stock_cost_job_back_to_work();

CREATE FUNCTION stock_cost_job_parts_changed() RETURNS trigger AS $$
BEGIN
    PERFORM stock_cost_job_changed(NEW.work_order_id, 'The parts taken out to the job changed after its cost was posted');
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER stock_cost_job_parts_changed AFTER INSERT ON stock_movements
    FOR EACH ROW WHEN (NEW.work_order_id IS NOT NULL AND NEW.kind IN ('consumed', 'put_back'))
    EXECUTE FUNCTION stock_cost_job_parts_changed();

CREATE FUNCTION stock_cost_job_hours_changed() RETURNS trigger AS $$
BEGIN
    PERFORM stock_cost_job_changed(NEW.work_order_id, 'The job''s hours changed after its cost was posted');
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER stock_cost_job_hours_added AFTER INSERT ON work_order_lines
    FOR EACH ROW WHEN (NEW.kind = 'labour')
    EXECUTE FUNCTION stock_cost_job_hours_changed();
CREATE TRIGGER stock_cost_job_hours_changed AFTER UPDATE ON work_order_lines
    FOR EACH ROW WHEN ((OLD.kind = 'labour' OR NEW.kind = 'labour')
                       AND (OLD.kind IS DISTINCT FROM NEW.kind OR OLD.quantity IS DISTINCT FROM NEW.quantity
                            OR OLD.withdrawn_at IS DISTINCT FROM NEW.withdrawn_at
                            OR OLD.declined_at IS DISTINCT FROM NEW.declined_at))
    EXECUTE FUNCTION stock_cost_job_hours_changed();
