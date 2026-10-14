-- rollback: safe
-- New tables and one nullable column. The previous release never reads them,
-- and approves a job the way it always did.
--
-- A quote is what the customer was shown, frozen.
--
-- Until now a customer's yes was a flag on a line, and a line can be edited:
-- the price the customer said yes to and the price on the invoice were the
-- same number only as long as nobody touched it. A yes given on a page that
-- showed no prices was later copied onto a line that had one.
--
-- So the lines the customer is asked about are copied into a numbered
-- version, and a decision points at that version. The lines of the job stay
-- editable; the version never is. Three tables, all append-only: nothing
-- here is updated or deleted, because the point is to answer "what exactly
-- did they agree to, and when" long after the answer is inconvenient.

CREATE TABLE quotes (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    -- 1, 2, 3 ... per order. Two people presenting at once get one each, not
    -- two copies of the same number.
    version       integer     NOT NULL CHECK (version > 0),
    net_minor     bigint      NOT NULL,
    vat_minor     bigint      NOT NULL,
    gross_minor   bigint      NOT NULL,
    presented_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    presented_at  timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (work_order_id, version)
);

CREATE TABLE quote_lines (
    id              uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id         uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    quote_id        uuid    NOT NULL REFERENCES quotes(id) ON DELETE CASCADE,
    -- The job line this was copied from; the copy is what was shown.
    line_id         uuid    REFERENCES work_order_lines(id) ON DELETE SET NULL,
    position        integer NOT NULL,
    kind            text    NOT NULL,
    description     text    NOT NULL,
    quantity        numeric(12,3) NOT NULL,
    unit_price_minor bigint NOT NULL,
    vat_rate_bp     integer NOT NULL,
    net_minor       bigint  NOT NULL,
    vat_minor       bigint  NOT NULL,
    UNIQUE (quote_id, position)
);

CREATE TABLE quote_decisions (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    quote_id      uuid        NOT NULL REFERENCES quotes(id) ON DELETE CASCADE,
    decision      text        NOT NULL CHECK (decision IN ('approved', 'declined')),
    -- On their own link, or by telephone or at the counter and written down
    -- by somebody at the workshop.
    via           text        NOT NULL CHECK (via IN ('link', 'phone', 'counter')),
    recorded_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    job_link_id   uuid        REFERENCES job_links(id) ON DELETE SET NULL,
    -- What was agreed, as the person said it. Must equal the version's total
    -- when the decision is a yes, so an approval is always of a number.
    agreed_gross_minor bigint,
    note          text,
    decided_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((via = 'link') = (recorded_by IS NULL)),
    CHECK (decision <> 'approved' OR agreed_gross_minor IS NOT NULL)
);
CREATE INDEX quote_decisions_quote_idx ON quote_decisions (quote_id, decided_at);

-- Which agreement a line was approved under.
ALTER TABLE work_order_lines
    ADD COLUMN quote_decision_id uuid REFERENCES quote_decisions(id) ON DELETE SET NULL;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['quotes', 'quote_lines', 'quote_decisions'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END
$$;

CREATE TRIGGER notify_change AFTER INSERT ON quotes
    FOR EACH ROW EXECUTE FUNCTION notify_change('job');
CREATE TRIGGER notify_change AFTER INSERT ON quote_decisions
    FOR EACH ROW EXECUTE FUNCTION notify_change('job', 'board');
