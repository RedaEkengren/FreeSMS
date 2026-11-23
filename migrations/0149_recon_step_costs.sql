-- rollback: safe
-- A new table the previous release never reads: a cost entered against a step
-- stays a cost on the car whether or not the step is shown with it.
--
-- Which cost entries belong to a reconditioning step (#236, #164), so the
-- plan can say what a step came to against what was agreed for it. The cost
-- itself is a stock cost entry like any other, kept apart from the tax
-- purchase price; this only points at it. Nothing is written over: a cost
-- entered wrong is reversed in the ledger, and the link stays.
CREATE TABLE recon_step_costs (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    step_id       uuid        NOT NULL REFERENCES recon_steps(id) ON DELETE RESTRICT,
    cost_entry_id uuid        NOT NULL UNIQUE REFERENCES stock_cost_entries(id) ON DELETE RESTRICT,
    created_at    timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX recon_step_costs_step_idx ON recon_step_costs (step_id);

ALTER TABLE recon_step_costs ENABLE ROW LEVEL SECURITY;
ALTER TABLE recon_step_costs FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON recon_step_costs
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON recon_step_costs FROM freesms_app;
GRANT SELECT, INSERT ON recon_step_costs TO freesms_app;
