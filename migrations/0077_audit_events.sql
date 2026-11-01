-- rollback: safe
-- A new table and a view the previous release does not read. Overrides of
-- a block are recorded here from now on rather than in
-- resource_block_changes, which the previous release only ever wrote and
-- never read; its rows stay, and are read through the view.
--
-- Who did what, when, from what to what and why, for every transition that
-- moves money, ownership or responsibility (#151), from one place. Many
-- such facts already have a home that is itself kept by the database --
-- a job's state changes (0042), a correction to a record (0049), a module
-- switched (0063), a payment and its reversal (0011), a signature and a
-- correction to a contract (0072) -- and are not written twice: the view
-- below reads them where they are. audit_events holds the rest, which had
-- nowhere: a person's roles changed, a booking overridden, an invoice
-- issued with lines the customer never agreed to, a payment far above what
-- was owed recorded anyway. Dealer and Rental add theirs here rather than
-- a table each.
CREATE TABLE audit_events (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id          uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- Two events in one request keep their order, whatever the clock says.
    seq              bigint      GENERATED ALWAYS AS IDENTITY,
    subject_kind     text        NOT NULL CHECK (subject_kind IN ('shop', 'user', 'resource_block', 'document', 'invoice',
                                                                  'payment', 'party', 'customer', 'vehicle', 'work_order')),
    subject_id       uuid        NOT NULL,
    event            text        NOT NULL CHECK (event ~ '^[a-z_]+\.[a-z_]+$'),
    from_state       text        CHECK (from_state IS NULL OR length(from_state) <= 200),
    to_state         text        CHECK (to_state IS NULL OR length(to_state) <= 200),
    -- Null for the system: a sweep, the outbox. on_behalf_of is the person
    -- whose action set it going, where known.
    actor            uuid        REFERENCES users(id) ON DELETE RESTRICT,
    on_behalf_of     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    at               timestamptz NOT NULL DEFAULT clock_timestamp(),
    reason           text        CHECK (reason IS NULL OR length(btrim(reason)) BETWEEN 1 AND 300),
    -- Set when an erasure took the reason away: it can name the person.
    reason_erased_at timestamptz,
    -- Ids, amounts, counts: never a name, an address or a number that is a
    -- person's. A reader may be shown less of it than is here (Go, by role).
    detail           jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(detail) = 'object'),
    CHECK (reason IS NULL OR reason_erased_at IS NULL),
    -- An override, a reversal and a decision against what the system said
    -- are not made without a reason.
    CHECK (reason IS NOT NULL OR reason_erased_at IS NOT NULL
           OR event NOT IN ('block.overridden', 'invoice.unagreed_lines_invoiced', 'payment.overpayment_confirmed'))
);
CREATE INDEX audit_events_subject_idx ON audit_events (subject_id, seq);

ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON audit_events
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
-- The default privileges (0018) give everything; revoke first (0071).
REVOKE ALL ON audit_events FROM freesms_app;
GRANT SELECT, INSERT ON audit_events TO freesms_app;
GRANT UPDATE (reason, reason_erased_at) ON audit_events TO freesms_app;

-- Append-only, whatever writes: the one change let through is an erasure
-- taking the reason away, and nothing else about the event.
CREATE FUNCTION audit_events_stay() RETURNS trigger AS $$
DECLARE
    kept audit_events;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.reason IS NOT NULL AND NEW.reason IS NULL AND NEW.reason_erased_at IS NOT NULL THEN
        kept := OLD;
        kept.reason := NULL;
        kept.reason_erased_at := NEW.reason_erased_at;
        IF NEW IS NOT DISTINCT FROM kept THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'an audit event is never changed or deleted' USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER audit_events_stay BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_stay();

-- Every transition, wherever it is kept, in one shape: what a subject's
-- timeline reads. As the caller, so row level security applies.
-- Corrections to records say which field, never what it was or became:
-- those values are personal data, read on the record's own page and
-- cleared by an erasure.
CREATE VIEW audit_trail WITH (security_invoker = true) AS
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
    SELECT p.shop_id, 'invoice', p.invoice_id, CASE WHEN p.reverses_id IS NULL THEN 'payment.recorded' ELSE 'payment.reversed' END,
           NULL, NULL, p.recorded_by, p.recorded_at, 0, NULL, false,
           jsonb_build_object('payment_id', p.id, 'amount_minor', p.amount_minor, 'method', p.method,
                              'reference', p.series || '-' || p.number, 'reverses_id', p.reverses_id),
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
    -- Overrides made before 0077, where they were kept then.
    SELECT c.shop_id, 'resource_block', c.block_id, 'block.overridden', NULL, c.change, c.by_user, c.at, 0, c.reason, false,
           jsonb_build_object('change', c.change, 'old_from', lower(c.old_period), 'old_to', upper(c.old_period),
                              'new_from', lower(c.new_period), 'new_to', upper(c.new_period)),
           'resource_block_changes'
      FROM resource_block_changes c;
GRANT SELECT ON audit_trail TO freesms_app;
