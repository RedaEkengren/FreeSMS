-- rollback: safe
-- A new table the previous release never reads. Links made since would stop
-- opening after a rollback, which a supplier reads as an expired link and the
-- shop answers by making another.
--
-- A link for the supplier a car is with (#236): one outside step, no login.
-- It shows the supplier the car and the step and lets them say it is done.
-- As the customer's links are: only the token's hash is kept, it is shown once,
-- it expires, and making a new one closes the earlier.
CREATE TABLE recon_supplier_links (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    step_id      uuid        NOT NULL REFERENCES recon_steps(id) ON DELETE RESTRICT,
    token_sha256 bytea       NOT NULL UNIQUE,
    created_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    CHECK (expires_at > created_at)
);
CREATE INDEX recon_supplier_links_step_idx ON recon_supplier_links (step_id);

ALTER TABLE recon_supplier_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE recon_supplier_links FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON recon_supplier_links
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON recon_supplier_links FROM freesms_app;
GRANT SELECT, INSERT, UPDATE ON recon_supplier_links TO freesms_app;
