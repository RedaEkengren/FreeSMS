-- rollback: safe
-- Triggers only, no table or column: the previous release's rows notify
-- as well, which nothing it runs listens for.
--
-- The home page and the one action queue (#157, #158) are recomputed
-- under the reader's permissions when 'actions' is heard (0088). Car sales'
-- and rental's items are made from these tables, which said nothing, so a
-- lead taken, a deal contracted, a rental checked out or a fleet car's
-- insurance recorded showed on another open screen only when it was
-- reloaded. A notification says only that something changed in a shop,
-- never what.
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON leads
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON lead_events
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON test_drives
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON test_drive_events
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON stock_units
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON stock_checks
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON stock_check_overrides
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON vehicle_sales
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON vehicle_sale_events
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON vehicle_sale_defect_answers
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON rental_bookings
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON rental_booking_events
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON rental_drivers
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON rental_licence_checks
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON rental_handovers
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON damage_cases
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON fleet_memberships
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON fleet_rental_reports
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON fleet_insurances
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON rental_gap_overrides
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON rental_permits
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON rental_permit_revocations
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
