-- rollback: safe
-- 0090 came in the same release, so the release before has neither the
-- table nor anything that reads it.
--
-- An exception opened for one technician (0090) is written with why, and
-- why can name them -- "covering the front desk while Anna is on leave".
-- Erasing that person takes it away, as it takes the reasons of the
-- audit events about them (0077): that the field was opened for them, by
-- whom, when and until when stays. The shop's profile is about every
-- technician, not one person, and keeps its reasons, as an event about
-- the shop does.
ALTER TABLE technician_view_exceptions
    ALTER COLUMN reason DROP NOT NULL,
    -- Set when an erasure took the reason away.
    ADD COLUMN reason_erased_at timestamptz,
    ADD CONSTRAINT technician_view_exceptions_reason_or_erased
        CHECK ((reason IS NULL) = (reason_erased_at IS NOT NULL));
GRANT UPDATE (reason, reason_erased_at) ON technician_view_exceptions TO freesms_app;

-- Still a change added, never one rewritten: the one change let through
-- is an erasure taking the reason away, and nothing else about the row.
CREATE FUNCTION technician_view_exceptions_stay() RETURNS trigger AS $$
DECLARE
    kept technician_view_exceptions;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.reason IS NOT NULL AND NEW.reason IS NULL AND NEW.reason_erased_at IS NOT NULL THEN
        kept := OLD;
        kept.reason := NULL;
        kept.reason_erased_at := NEW.reason_erased_at;
        IF NEW IS NOT DISTINCT FROM kept THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'a technician''s exception is never changed or deleted' USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER technician_view_exceptions_stay BEFORE UPDATE OR DELETE ON technician_view_exceptions
    FOR EACH ROW EXECUTE FUNCTION technician_view_exceptions_stay();

-- The trail says the reason was erased, as it does for an audit event.
CREATE OR REPLACE VIEW audit_trail WITH (security_invoker = true) AS
    SELECT e.shop_id, e.subject_kind, e.subject_id, e.event, e.from_state, e.to_state, e.actor, e.at, e.seq,
           e.reason, e.reason_erased_at IS NOT NULL AS reason_erased, e.detail, 'audit_events' AS source
      FROM audit_events e
    UNION ALL
    SELECT s.shop_id, 'work_order', s.work_order_id, 'job.state_changed', s.from_state, s.to_state, s.changed_by, s.at, 0,
           NULL, false, '{}'::jsonb, 'work_order_state_changes'
      FROM work_order_state_changes s
    UNION ALL
    SELECT r.shop_id, CASE r.subject WHEN 'merge' THEN 'customer' WHEN 'ownership' THEN 'vehicle'
                                     WHEN 'registration' THEN 'vehicle' ELSE r.subject END,
           r.subject_id, 'record.corrected', NULL, NULL, r.by_user, r.at, 0,
           r.reason, false, jsonb_build_object('field', r.field, 'record', r.subject), 'record_changes'
      FROM record_changes r
    UNION ALL
    SELECT m.shop_id, 'shop', m.shop_id, CASE WHEN m.enabled THEN 'module.enabled' ELSE 'module.disabled' END,
           NULL, m.module, m.by_user, m.at, 0, nullif(m.reason, ''), false, jsonb_build_object('module', m.module),
           'shop_module_changes'
      FROM shop_module_changes m
    UNION ALL
    SELECT p.shop_id, CASE WHEN p.invoice_id IS NOT NULL THEN 'invoice' WHEN p.deposit_id IS NOT NULL THEN 'deposit' ELSE 'payout' END,
           coalesce(p.invoice_id, p.deposit_id, p.payout_id),
           CASE WHEN p.reverses_id IS NOT NULL THEN 'payment.reversed'
                WHEN p.method = 'deposit' THEN 'payment.applied'
                WHEN p.payout_id IS NOT NULL THEN 'payment.paid_out'
                WHEN p.refunds_id IS NOT NULL OR p.amount_minor < 0 THEN 'payment.refunded'
                ELSE 'payment.recorded' END,
           NULL, p.direction, p.recorded_by, p.recorded_at, 0, p.reason, false,
           jsonb_build_object('payment_id', p.id, 'amount_minor', p.amount_minor, 'method', p.method,
                              'reference', p.series || '-' || p.number, 'reverses_id', p.reverses_id, 'refunds_id', p.refunds_id),
           'invoice_payments'
      FROM invoice_payments p
    UNION ALL
    SELECT g.shop_id, 'document', g.document_id, 'document.signed', NULL, g.method, g.recorded_by, g.signed_at, 0, NULL, false,
           jsonb_build_object('document_party_id', g.document_party_id, 'sha256', encode(g.document_sha256, 'hex')),
           'document_signatures'
      FROM document_signatures g
    UNION ALL
    SELECT d.shop_id, 'document', d.supersedes_id, 'document.superseded', NULL, NULL, d.created_by, d.created_at, 0, NULL, false,
           jsonb_build_object('superseded_by', d.id), 'document_records'
      FROM document_records d WHERE d.supersedes_id IS NOT NULL
    UNION ALL
    SELECT c.shop_id, 'resource_block', c.block_id, 'block.overridden', NULL, c.change, c.by_user, c.at, 0, c.reason, false,
           jsonb_build_object('change', c.change, 'old_from', lower(c.old_period), 'old_to', upper(c.old_period),
                              'new_from', lower(c.new_period), 'new_to', upper(c.new_period)),
           'resource_block_changes'
      FROM resource_block_changes c
    UNION ALL
    SELECT m.shop_id, 'payment', m.payment_id, 'payment.matched', NULL, NULL, m.matched_by, m.matched_at, 0, NULL, false,
           jsonb_build_object('statement_line_id', m.statement_line_id), 'payment_matches'
      FROM payment_matches m
    UNION ALL
    SELECT m.shop_id, 'payment', m.payment_id, 'payment.match_undone', NULL, NULL, m.undone_by, m.undone_at, 0, NULL, false,
           jsonb_build_object('statement_line_id', m.statement_line_id), 'payment_matches'
      FROM payment_matches m WHERE m.undone_at IS NOT NULL
    UNION ALL
    SELECT c.shop_id, 'shop', c.shop_id, 'technician_view.changed', NULL, CASE WHEN c.shown THEN 'shown' ELSE 'hidden' END,
           c.changed_by, c.changed_at, c.seq, c.reason, false, jsonb_build_object('field', c.field),
           'technician_view_changes'
      FROM technician_view_changes c
    UNION ALL
    SELECT e.shop_id, 'user', e.user_id, CASE WHEN e.granted THEN 'technician_view.granted' ELSE 'technician_view.ended' END,
           NULL, e.field, e.by_user, e.at, e.seq, e.reason, e.reason_erased_at IS NOT NULL,
           jsonb_build_object('field', e.field, 'until', e.until), 'technician_view_exceptions'
      FROM technician_view_exceptions e;
