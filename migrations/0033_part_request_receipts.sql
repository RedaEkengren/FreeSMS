-- rollback: safe
-- A column with a default and a table the previous release never reads.
-- arrived_at keeps its meaning -- set when everything asked for has come --
-- so the previous release reads a part-delivered request as still open,
-- which it is.
--
-- A part request was a sentence and a tick. Real deliveries arrive in
-- pieces: two of four brake discs on Tuesday and the rest on Friday, and the
-- shop has to know which car can be finished. A request now carries a
-- quantity, and each arrival is a row against it: how many, whether it came
-- in a delivery or off the shelf, who took it in and when -- the fact
-- somebody will hold up against a supplier's delivery note.
ALTER TABLE part_requests ADD COLUMN quantity numeric(12,3) NOT NULL DEFAULT 1 CHECK (quantity > 0);

CREATE TABLE part_request_receipts (
    id          uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid          NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    request_id  uuid          NOT NULL REFERENCES part_requests(id) ON DELETE CASCADE,
    -- More than was asked for is recorded as it came: five when four were
    -- ordered is a fact, not an error.
    quantity    numeric(12,3) NOT NULL CHECK (quantity > 0),
    -- A delivery, or the part was on the shelf after all. Either finishes the
    -- request; only the first is something a supplier is asked about.
    source      text          NOT NULL CHECK (source IN ('delivery', 'shelf')),
    received_at timestamptz   NOT NULL DEFAULT clock_timestamp(),
    -- Null only for the receipts written below for requests that arrived
    -- before receipts existed and did not record by whom.
    received_by uuid          REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX part_request_receipts_request_idx ON part_request_receipts (request_id);

-- Before row level security is on, which would refuse rows written with no
-- shop chosen.
--
-- A request that arrived before this was a request for one that arrived
-- whole. Written as a receipt, so what is outstanding reads the same either
-- way.
INSERT INTO part_request_receipts (shop_id, request_id, quantity, source, received_at, received_by)
SELECT shop_id, id, quantity, 'delivery', arrived_at, arrived_by
FROM part_requests WHERE arrived_at IS NOT NULL;

ALTER TABLE part_request_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE part_request_receipts FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON part_request_receipts
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT, DELETE ON part_request_receipts TO freesms_app;

CREATE FUNCTION part_request_receipts_are_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'what arrived is not edited; record what happened next'
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER part_request_receipts_append_only
    BEFORE UPDATE ON part_request_receipts
    FOR EACH ROW EXECUTE FUNCTION part_request_receipts_are_append_only();
