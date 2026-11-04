-- rollback: safe
-- The trail's view gains rows read from tables that are already there; the
-- previous release reads the same columns and shows the new events by name.
--
-- Rental's transitions in the one trail (#151): a car handed out and
-- returned, a deposit asked for and a hold taken or let go, a return charge
-- confirmed, told or withdrawn, and a fine passed to a renter, kept or paid.
-- Each already has a home kept by the database (0122, 0138, 0139, 0141), so
-- it is read from there and not written twice.
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
      FROM technician_view_exceptions e
    UNION ALL
    SELECT h.shop_id, 'rental_booking', h.booking_id,
           CASE h.kind WHEN 'check_out' THEN 'rental.handed_out' ELSE 'rental.returned' END,
           NULL, h.kind, h.by_user, h.at, 0, NULL, false,
           jsonb_build_object('protocol_id', h.protocol_id, 'renter_present', h.renter_present,
                              'renter_accepted', h.renter_accepted), 'rental_handovers'
      FROM rental_handovers h
    UNION ALL
    SELECT e.shop_id, 'rental_booking', d.booking_id, 'rental_deposit.' || e.kind, NULL, e.method, e.by_user, e.at, e.seq,
           e.reason, false, jsonb_build_object('amount_minor', e.amount_minor, 'method', e.method, 'valid_until', e.valid_until),
           'rental_deposit_events'
      FROM rental_deposit_events e JOIN rental_deposits d ON d.id = e.rental_deposit_id
    UNION ALL
    SELECT c.shop_id, 'rental_booking', c.booking_id, 'rental_charge.confirmed', NULL, c.kind, c.confirmed_by, c.confirmed_at, 0,
           NULL, false, jsonb_build_object('charge_id', c.id, 'kind', c.kind, 'proposed', c.proposed), 'rental_charges'
      FROM rental_charges c
    UNION ALL
    SELECT e.shop_id, 'rental_booking', c.booking_id, 'rental_charge.' || e.kind, NULL, e.channel, e.by_user, e.at, 0,
           e.reason, false, jsonb_build_object('charge_id', c.id, 'kind', c.kind, 'channel', e.channel), 'rental_charge_events'
      FROM rental_charge_events e JOIN rental_charges c ON c.id = e.charge_id
    UNION ALL
    SELECT e.shop_id, 'vehicle_fine', e.fine_id,
           CASE e.kind WHEN 'renter' THEN 'fine.passed_to_renter' WHEN 'shop' THEN 'fine.kept' ELSE 'fine.paid' END,
           NULL, e.kind, e.by_user, e.at, e.seq, e.reason, false,
           jsonb_build_object('booking_id', e.booking_id, 'charge_id', e.charge_id), 'vehicle_fine_events'
      FROM vehicle_fine_events e;
