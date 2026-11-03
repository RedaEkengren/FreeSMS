-- rollback: safe
-- New tables, and new columns on invoice_payments that the previous release
-- neither writes nor needs: each is nullable or derived, and every row it
-- writes -- a payment against an invoice -- is one these constraints accept.
-- Rows against a deposit or a payout it would list nowhere, as it has no
-- page that could have made them.
--
-- The payment journal (#152). invoice_payments was already a good ledger --
-- a row per payment, negative for a refund or a reversal, a gap-free series,
-- immutable but for the export marker -- and it is extended rather than
-- replaced. FreeSMS still never handles money: every row is money that moved
-- in the shop's own channels, recorded.
--
-- What a payment is against: an invoice (any source, #146), a deposit, or a
-- payout. A deposit is money held against something not yet owed -- a car
-- reserved, a rental -- and is not income (plan, section 1.8); a payout is
-- money paid out to a party -- a private seller for their car. Exactly one.
CREATE TABLE deposits (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    party_id     uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    purpose      text        NOT NULL CHECK (purpose IN ('reservation', 'rental', 'down_payment', 'other')),
    -- What it is held against. Their tables come with their modules.
    subject_kind text        CHECK (subject_kind IS NULL OR subject_kind IN ('sale', 'rental_booking', 'reservation')),
    subject_id   uuid,
    note         text        CHECK (note IS NULL OR length(note) <= 300),
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by   uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((subject_kind IS NULL) = (subject_id IS NULL))
);
CREATE INDEX deposits_party_idx ON deposits (party_id);

CREATE TABLE payouts (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    party_id     uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    purpose      text        NOT NULL CHECK (purpose IN ('purchase', 'consignment', 'other')),
    subject_kind text        CHECK (subject_kind IS NULL OR subject_kind IN ('purchase', 'consignment')),
    subject_id   uuid,
    note         text        CHECK (note IS NULL OR length(note) <= 300),
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by   uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((subject_kind IS NULL) = (subject_id IS NULL))
);
CREATE INDEX payouts_party_idx ON payouts (party_id);

ALTER TABLE invoice_payments
    ALTER COLUMN invoice_id DROP NOT NULL,
    ADD COLUMN deposit_id  uuid REFERENCES deposits(id) ON DELETE RESTRICT,
    ADD COLUMN payout_id   uuid REFERENCES payouts(id) ON DELETE RESTRICT,
    -- In or out, said rather than read off a sign; the sign rules stay as
    -- they were -- money in positive, a refund or a reversal negative -- so
    -- it is derived from them, and a release that does not know it cannot
    -- write it wrong.
    ADD COLUMN direction   text GENERATED ALWAYS AS (CASE WHEN amount_minor + rounding_minor < 0 THEN 'out' ELSE 'in' END) STORED,
    -- A refund is its own row, linked to what it gives back; never an edit.
    ADD COLUMN refunds_id  uuid REFERENCES invoice_payments(id) ON DELETE RESTRICT,
    -- Who carried it and their own id for it: the bank's line, the Swish
    -- payment, the terminal's receipt. One line is recorded once.
    ADD COLUMN provider    text CHECK (provider IS NULL OR provider ~ '^[a-z][a-z0-9_]{0,39}$'),
    ADD COLUMN external_id text CHECK (external_id IS NULL OR length(external_id) BETWEEN 1 AND 200),
    -- Why it was reversed or refunded.
    ADD COLUMN reason      text CHECK (reason IS NULL OR length(btrim(reason)) BETWEEN 1 AND 300),
    -- A submission repeated from a phone or a double click finds its row.
    ADD COLUMN request_key text CHECK (request_key IS NULL OR length(request_key) <= 100),
    ADD CONSTRAINT invoice_payments_one_target CHECK (num_nonnulls(invoice_id, deposit_id, payout_id) = 1),
    ADD CONSTRAINT invoice_payments_payout_out CHECK (payout_id IS NULL OR amount_minor < 0 OR reverses_id IS NOT NULL),
    ADD CONSTRAINT invoice_payments_provider_id CHECK ((provider IS NULL) = (external_id IS NULL)),
    ADD CONSTRAINT invoice_payments_refund_or_reversal CHECK (refunds_id IS NULL OR reverses_id IS NULL);
CREATE UNIQUE INDEX invoice_payments_external_idx ON invoice_payments (shop_id, provider, external_id) WHERE external_id IS NOT NULL;
CREATE UNIQUE INDEX invoice_payments_request_idx ON invoice_payments (shop_id, request_key) WHERE request_key IS NOT NULL;
CREATE INDEX invoice_payments_deposit_idx ON invoice_payments (deposit_id) WHERE deposit_id IS NOT NULL;
CREATE INDEX invoice_payments_payout_idx ON invoice_payments (payout_id) WHERE payout_id IS NOT NULL;
CREATE INDEX invoice_payments_refunds_idx ON invoice_payments (refunds_id) WHERE refunds_id IS NOT NULL;

-- Immutable as before, but for the export marker. A BEFORE trigger sees a
-- generated column not yet computed, so direction -- which is the amount's
-- sign and changes only with it -- is left out of the comparison.
CREATE OR REPLACE FUNCTION invoice_payments_are_immutable() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.accounting_export_id IS DISTINCT FROM OLD.accounting_export_id
       AND OLD.accounting_export_id IS NULL
       AND (to_jsonb(NEW) - 'accounting_export_id' - 'direction') = (to_jsonb(OLD) - 'accounting_export_id' - 'direction')
    THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION
        'a recorded payment cannot be changed or deleted (% on payment %); reverse it with another',
        TG_OP, coalesce(OLD.id::text, '?')
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

-- A deposit applied to what came to be owed is money moved inside the
-- shop's books, not money that arrived: its own way of paying.
ALTER TABLE invoice_payments DROP CONSTRAINT invoice_payments_method_check;
ALTER TABLE invoice_payments ADD CONSTRAINT invoice_payments_method_check
    CHECK (method IN ('bank', 'cash', 'card', 'swish', 'other', 'deposit'));

-- A refund gives back part or all of one payment, against the same thing,
-- never more than it was, however many refunds there are.
CREATE FUNCTION payment_refunds_stay_within() RETURNS trigger AS $$
DECLARE
    orig invoice_payments;
    given bigint;
BEGIN
    IF NEW.refunds_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT * INTO orig FROM invoice_payments WHERE id = NEW.refunds_id FOR UPDATE;
    IF orig.amount_minor <= 0 OR orig.reverses_id IS NOT NULL OR orig.refunds_id IS NOT NULL THEN
        RAISE EXCEPTION 'only money received is refunded' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.amount_minor >= 0
       OR NEW.invoice_id IS DISTINCT FROM orig.invoice_id OR NEW.deposit_id IS DISTINCT FROM orig.deposit_id
       OR NEW.payout_id IS DISTINCT FROM orig.payout_id THEN
        RAISE EXCEPTION 'a refund gives money back against what it was received for' USING ERRCODE = 'check_violation';
    END IF;
    -- What is refunded already, less any refund since reversed.
    SELECT coalesce(-sum(r.amount_minor), 0) - coalesce((SELECT sum(x.amount_minor) FROM invoice_payments x
                                                          WHERE x.reverses_id IN (SELECT id FROM invoice_payments WHERE refunds_id = orig.id)), 0)
      INTO given FROM invoice_payments r WHERE r.refunds_id = orig.id;
    IF given - NEW.amount_minor > orig.amount_minor THEN
        RAISE EXCEPTION 'a refund of more than was paid' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER payment_refunds_stay_within BEFORE INSERT ON invoice_payments
    FOR EACH ROW EXECUTE FUNCTION payment_refunds_stay_within();

-- Reconciliation: the bank's statement, line by line, typed or imported,
-- and which payment each line is. A match made wrongly is undone by a row
-- saying so, not deleted.
CREATE TABLE bank_statement_lines (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    booked_on   date        NOT NULL,
    amount_minor bigint     NOT NULL CHECK (amount_minor <> 0),
    reference   text        CHECK (reference IS NULL OR length(reference) <= 200),
    source      text        NOT NULL CHECK (source IN ('typed', 'imported')),
    -- The bank's own id for the line, where it has one: imported twice, it
    -- is one line.
    external_id text        CHECK (external_id IS NULL OR length(external_id) BETWEEN 1 AND 200),
    created_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by  uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX bank_statement_lines_external_idx ON bank_statement_lines (shop_id, external_id) WHERE external_id IS NOT NULL;

CREATE TABLE payment_matches (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id           uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    payment_id        uuid        NOT NULL REFERENCES invoice_payments(id) ON DELETE RESTRICT,
    statement_line_id uuid        NOT NULL REFERENCES bank_statement_lines(id) ON DELETE RESTRICT,
    matched_at        timestamptz NOT NULL DEFAULT clock_timestamp(),
    matched_by        uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    undone_at         timestamptz,
    undone_by         uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((undone_at IS NULL) = (undone_by IS NULL))
);
CREATE UNIQUE INDEX payment_matches_payment_idx ON payment_matches (payment_id) WHERE undone_at IS NULL;
CREATE UNIQUE INDEX payment_matches_line_idx ON payment_matches (statement_line_id) WHERE undone_at IS NULL;

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['deposits', 'payouts', 'bank_statement_lines', 'payment_matches'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        -- The default privileges (0018) give everything; revoke first (0071).
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END;
$$;
GRANT UPDATE (undone_at, undone_by) ON payment_matches TO freesms_app;

-- The trail reads payments of every target, with why a reversal or a
-- refund was made.
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
      FROM payment_matches m WHERE m.undone_at IS NOT NULL;
