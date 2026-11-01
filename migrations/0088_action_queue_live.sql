-- rollback: safe
-- Triggers only, no table or column: the previous release's rows notify
-- as well, which nothing it runs listens for.
--
-- The one action queue (#158) is recomputed under the reader's
-- permissions whenever something it is made from changes. The workshop's
-- items already notify through 'board' and 'parts' (0037); Core's -- a
-- call given up or done by hand, a document signed or replaced, a payment
-- flow used or checked -- notify 'actions', and the queue listens for it.
-- A notification says only that something changed, never what.
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON document_records
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON document_signatures
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON payment_flows
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON payment_flow_checks
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON cash_invoices
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');
