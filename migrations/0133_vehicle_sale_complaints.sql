-- rollback: safe
-- New tables and nothing the previous release reads.
--
-- A buyer's complaint about a car they bought (#181): it belongs to the
-- sale, and reads the frozen delivery (#179) for whether the consumer
-- law's presumption and the dealer's liability still run. It shows the
-- dates and decides nothing about liability (G2). What happens to it is
-- rows, each by a person at a time with a note, never changed.
CREATE TABLE vehicle_sale_complaints (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    number       bigint      GENERATED ALWAYS AS IDENTITY,
    sale_id      uuid        NOT NULL REFERENCES vehicle_sales(id) ON DELETE RESTRICT,
    received_at  timestamptz NOT NULL,
    channel      text        NOT NULL CHECK (channel IN ('in_person', 'phone', 'email', 'letter', 'other')),
    description  text        NOT NULL CHECK (length(btrim(description)) BETWEEN 1 AND 2000),
    -- When the buyer says they noticed it: the two-month rule counts from
    -- here, as a reminder, not a decision.
    noticed_on   date,
    request_key  text        CHECK (request_key IS NULL OR length(request_key) <= 100),
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (received_at <= created_at + interval '1 hour')
);
CREATE UNIQUE INDEX vehicle_sale_complaints_request_idx ON vehicle_sale_complaints (shop_id, request_key) WHERE request_key IS NOT NULL;
CREATE INDEX vehicle_sale_complaints_sale_idx ON vehicle_sale_complaints (sale_id);

CREATE TABLE vehicle_sale_complaint_events (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    complaint_id uuid        NOT NULL REFERENCES vehicle_sale_complaints(id) ON DELETE RESTRICT,
    seq          bigint      GENERATED ALWAYS AS IDENTITY,
    status       text        NOT NULL CHECK (status IN ('received', 'assessed', 'remedy_offered', 'remedy_accepted',
                                                        'remedy_refused', 'remedy_done', 'closed', 'referred_arn')),
    remedy       text        CHECK (remedy IS NULL OR remedy IN ('repair', 'replacement', 'price_reduction', 'rescission')),
    note         text        CHECK (note IS NULL OR length(note) <= 1000),
    at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user      uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (status NOT IN ('remedy_offered', 'remedy_accepted', 'remedy_refused', 'remedy_done') OR remedy IS NOT NULL)
);
CREATE INDEX vehicle_sale_complaint_events_idx ON vehicle_sale_complaint_events (complaint_id, seq);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['vehicle_sale_complaints', 'vehicle_sale_complaint_events'] LOOP
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
