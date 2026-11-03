-- rollback: safe
-- New tables and a view the previous release does not read, and the
-- audit trail with two more sources, which the previous release reads and
-- shows as it shows any event.
--
-- What a technician sees (#225). By default a job's work, its vehicle and
-- registration, its parts and its time, and nothing else: no price, no
-- rate, no customer, no invoice. Whoever runs the shop opens a field for
-- every technician (the shop's profile) or for one person (an exception,
-- with an end if they like), always with a reason. Each change is a row:
-- where a field stands is its latest, and an exception past its end stops
-- applying at the next request, with nothing to sweep it. The fields are
-- the six the issue names and no others: a personal identity number, an
-- identity document, due diligence, payment details, the customer
-- register and the shop's finance are not fields here, and the CHECK is
-- the last line that keeps them out.
CREATE TABLE technician_view_changes (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    seq        bigint      GENERATED ALWAYS AS IDENTITY,
    field      text        NOT NULL CHECK (field IN ('cost_ceiling', 'line_prices', 'hourly_rate',
                                                     'customer_name', 'customer_phone', 'invoice_status')),
    shown      boolean     NOT NULL,
    reason     text        NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 300),
    changed_by uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    changed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX technician_view_changes_idx ON technician_view_changes (shop_id, field, seq);

CREATE TABLE technician_view_exceptions (
    id       uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id  uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    seq      bigint      GENERATED ALWAYS AS IDENTITY,
    user_id  uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    field    text        NOT NULL CHECK (field IN ('cost_ceiling', 'line_prices', 'hourly_rate',
                                                   'customer_name', 'customer_phone', 'invoice_status')),
    -- Granted, or the grant ended by hand before its end.
    granted  boolean     NOT NULL,
    -- When a grant stops applying by itself; null for until it is ended.
    until    timestamptz,
    reason   text        NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 300),
    by_user  uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (granted OR until IS NULL)
);
CREATE INDEX technician_view_exceptions_idx ON technician_view_exceptions (user_id, field, seq);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['technician_view_changes', 'technician_view_exceptions'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        -- The default privileges (0018) give everything; revoke first (0071).
        -- A change is a row added, never one rewritten.
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END;
$$;

-- What is open now: the shop's profile for every technician (user_id
-- null), and each person's exceptions not ended and not past their end.
CREATE VIEW technician_fields_open WITH (security_invoker = true) AS
    SELECT p.shop_id, NULL::uuid AS user_id, p.field
      FROM (SELECT DISTINCT ON (shop_id, field) shop_id, field, shown
              FROM technician_view_changes ORDER BY shop_id, field, seq DESC) p
     WHERE p.shown
    UNION ALL
    SELECT e.shop_id, e.user_id, e.field
      FROM (SELECT DISTINCT ON (user_id, field) shop_id, user_id, field, granted, until
              FROM technician_view_exceptions ORDER BY user_id, field, seq DESC) e
     WHERE e.granted AND (e.until IS NULL OR e.until > clock_timestamp());
GRANT SELECT ON technician_fields_open TO freesms_app;

-- "Ask the front desk to contact the customer": a finding a technician
-- reports without being shown who the customer is or their number.
ALTER TABLE findings ADD COLUMN asks_contact boolean NOT NULL DEFAULT false;

-- The trail reads both, with why each was made.
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
           NULL, e.field, e.by_user, e.at, e.seq, e.reason, false,
           jsonb_build_object('field', e.field, 'until', e.until), 'technician_view_exceptions'
      FROM technician_view_exceptions e;
