-- Row level security: the backstop under the application's own filtering.
--
-- The application always scopes its queries by shop. This exists for the day
-- it does not -- a query written in two years that forgets the predicate, a
-- join that widens without anyone noticing. Postgres then refuses to return
-- the rows regardless of what the query asked for.
--
-- Three details decide whether this actually protects anything:
--
--   1. FORCE, not just ENABLE. A table's owner bypasses ordinary row level
--      security. Migrations run as the owner, so without FORCE an application
--      connecting as that same role would have no policy applied at all --
--      and everything would look like it worked.
--
--   2. current_setting(..., true). The second argument makes a missing
--      setting return NULL instead of raising. NULL in the comparison makes
--      the policy match nothing, so a request that failed to establish its
--      scope sees no rows rather than every row. It fails closed.
--
--   3. nullif(..., ''). An empty string would reach ''::uuid and raise. A
--      blank setting is a bug, and a bug should deny, not error.
--
-- The failure mode to remember: a scope that was never set looks exactly like
-- a shop with no data. Empty results are the symptom of a wiring mistake here,
-- not evidence that the database is empty.

-- A role with no DDL rights, for the application to run as. It cannot log in;
-- the owner switches to it per transaction with SET LOCAL ROLE, so there is
-- still one connection string and one password to manage.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'redasms_app') THEN
        CREATE ROLE redasms_app NOLOGIN;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO redasms_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO redasms_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO redasms_app;

-- schema_migrations is the migrator's bookkeeping. The application has no
-- business reading or writing it.
REVOKE ALL ON schema_migrations FROM redasms_app;

-- Let the owner become the application role.
DO $$
BEGIN
    EXECUTE format('GRANT redasms_app TO %I', current_user);
END
$$;

-- Every table carrying shop_id gets the same treatment. Tables added later
-- need their own policy; the integration tests fail if one is missing, which
-- is what stops this from being a thing everyone forgets.
DO $$
DECLARE
    t text;
BEGIN
    FOR t IN
        SELECT c.table_name
        FROM information_schema.columns c
        JOIN pg_tables p ON p.tablename = c.table_name AND p.schemaname = 'public'
        WHERE c.table_schema = 'public' AND c.column_name = 'shop_id'
        ORDER BY c.table_name
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$
            CREATE POLICY shop_isolation ON %I
                USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
                WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
        $p$, t);
    END LOOP;
END
$$;

-- The shops table itself: a request may only see its own shop.
ALTER TABLE shops ENABLE ROW LEVEL SECURITY;
ALTER TABLE shops FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON shops
    USING (id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (id = nullif(current_setting('app.current_shop', true), '')::uuid);
