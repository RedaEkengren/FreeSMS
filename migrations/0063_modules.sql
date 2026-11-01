-- rollback: safe
-- A new table and a view the previous release never reads.
--
-- Which of Workshop, Dealer and Rental a shop runs (#143). Switching one on or
-- off is something that happens, more than once over a shop's life, and who
-- did it and why is the question somebody asks when the rental calendar has
-- gone: so it is a row per change, never a flag written over. The current set
-- is derived from the latest change per module, in shop_modules below.
--
-- Append-only, like the payments: a change is taken back with another change.
CREATE TABLE shop_module_changes (
    id       uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id  uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    module   text        NOT NULL CHECK (module IN ('workshop', 'dealer', 'rental')),
    enabled  boolean     NOT NULL,
    -- Null for the change this migration writes, which nobody made.
    by_user  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    reason   text        NOT NULL DEFAULT '' CHECK (length(reason) <= 500),
    at       timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX shop_module_changes_latest_idx ON shop_module_changes (shop_id, module, at DESC);

-- Every shop there is was set up as a workshop, because there was nothing
-- else to be. Written before row level security is switched on, as the
-- migration's own change.
INSERT INTO shop_module_changes (shop_id, module, enabled, reason, at)
SELECT id, 'workshop', true, 'Every shop before modules was a workshop', created_at
  FROM shops;

ALTER TABLE shop_module_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE shop_module_changes FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON shop_module_changes
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON shop_module_changes FROM freesms_app;
GRANT SELECT, INSERT ON shop_module_changes TO freesms_app;

CREATE FUNCTION shop_module_changes_are_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION
        'a module change cannot be changed or deleted (% on %); make another change instead',
        TG_OP, coalesce(OLD.id::text, '?')
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER shop_module_changes_immutable
    BEFORE UPDATE OR DELETE ON shop_module_changes
    FOR EACH ROW EXECUTE FUNCTION shop_module_changes_are_immutable();

-- The modules a shop runs now, and since when.
--
-- A shop with no change recorded at all runs the workshop. The backfill above
-- covers every shop that exists; this covers a shop set up by the release
-- before this one after it had run -- a rollback by image, then setup -- which
-- would otherwise come back to a release that knows modules with none, and
-- lose its workshop for want of a row it could not have written. Once a shop
-- has any change, only its changes count.
--
-- security_invoker, so that row level security on the tables underneath
-- applies to whoever reads it rather than to the view's owner.
CREATE VIEW shop_modules WITH (security_invoker = true) AS
SELECT shop_id, module, at AS since, by_user
  FROM (SELECT DISTINCT ON (shop_id, module) shop_id, module, enabled, at, by_user
          FROM shop_module_changes
         ORDER BY shop_id, module, at DESC, id) latest
 WHERE enabled
UNION ALL
SELECT s.id, 'workshop', s.created_at, NULL
  FROM shops s
 WHERE NOT EXISTS (SELECT 1 FROM shop_module_changes c WHERE c.shop_id = s.id);
GRANT SELECT ON shop_modules TO freesms_app;
