-- rollback: safe
-- A new table the previous release never reads. Links made since would stop
-- opening after a rollback, which the customer reads as an expired link and
-- the front desk answers by making a new one.
--
-- A link to the job itself, for the customer to see where their car is:
-- being worked on, waiting for a part, ready to collect, and once invoiced
-- what is owed. Until now the only link a customer had was to an
-- inspection, so a job without one had nothing to send, and "your car is
-- ready" could only be said on the telephone.
--
-- As the inspection links are: the token's hash is kept, never the token; it
-- is shown once, expires, and can be closed.
CREATE TABLE job_links (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    token_sha256  bytea       UNIQUE,
    created_by    uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    CHECK (expires_at > created_at)
);
CREATE INDEX job_links_order_idx ON job_links (work_order_id);

ALTER TABLE job_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_links FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON job_links
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
GRANT SELECT, INSERT, UPDATE, DELETE ON job_links TO freesms_app;
