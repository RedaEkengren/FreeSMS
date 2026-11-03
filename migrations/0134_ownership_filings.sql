-- rollback: safe
-- New tables and nothing the previous release reads.
--
-- A change of ownership filed with Transportstyrelsen (#180): seller to
-- dealer at a purchase, dealer to buyer at a sale. There is no public
-- API, so a person files it and records here what happened; an adapter
-- (#214) can later do the same through the same interface. The control
-- number the filing needs is never kept: in manual mode the person uses
-- it at Transportstyrelsen and FreeSMS records only that it was in hand.
-- Only a confirmed filing changes the ownership history, once.
CREATE TABLE ownership_filings (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    subject_kind text        NOT NULL CHECK (subject_kind IN ('purchase', 'sale')),
    subject_id   uuid        NOT NULL,
    vehicle_id   uuid        NOT NULL REFERENCES vehicles(id) ON DELETE RESTRICT,
    -- Who it passes from and to; NULL is the shop itself. A consigned car
    -- passes from its principal straight to the buyer.
    from_party_id uuid       REFERENCES parties(id) ON DELETE RESTRICT,
    to_party_id   uuid       REFERENCES parties(id) ON DELETE RESTRICT,
    -- The day the ownership passes.
    passes_on    date        NOT NULL,
    adapter      text        NOT NULL DEFAULT 'manual' CHECK (adapter IN ('manual')),
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (num_nonnulls(from_party_id, to_party_id) >= 1),
    UNIQUE (subject_kind, subject_id)
);

CREATE TABLE ownership_filing_events (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    filing_id    uuid        NOT NULL REFERENCES ownership_filings(id) ON DELETE RESTRICT,
    seq          bigint      GENERATED ALWAYS AS IDENTITY,
    status       text        NOT NULL CHECK (status IN ('prepared', 'filed', 'confirmed', 'rejected')),
    -- The manual checklist, as answered when filed: the control number
    -- in hand (never the number), leasing consent where it is needed,
    -- retention of title on a credit sale.
    control_number_in_hand boolean,
    leasing_consent        boolean,
    retention_of_title     boolean,
    -- Transportstyrelsen's own reference for the confirmation, or why it
    -- was rejected.
    reference    text        CHECK (reference IS NULL OR length(reference) BETWEEN 1 AND 100),
    note         text        CHECK (note IS NULL OR length(note) <= 500),
    at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user      uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (status <> 'confirmed' OR reference IS NOT NULL),
    CHECK (status <> 'rejected' OR note IS NOT NULL)
);
-- Confirmed once: a retry finds it confirmed and changes nothing more.
CREATE UNIQUE INDEX ownership_filing_events_confirmed_once ON ownership_filing_events (filing_id) WHERE status = 'confirmed';
CREATE INDEX ownership_filing_events_idx ON ownership_filing_events (filing_id, seq);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['ownership_filings', 'ownership_filing_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
        EXECUTE format($p$CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON %I
            FOR EACH ROW EXECUTE FUNCTION notify_change('actions')$p$, t);
    END LOOP;
END;
$$;

SELECT widen_text_check('audit_events', 'audit_events_subject_kind_check', 'subject_kind', ARRAY['ownership_filing']);
