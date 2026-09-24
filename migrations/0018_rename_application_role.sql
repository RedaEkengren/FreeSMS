-- The project was renamed from RedaSMS to FreeSMS.
--
-- Everything outside this directory was renamed with it. Migrations could not
-- be: an applied migration whose content changes is refused at startup,
-- because it means the database and the repository disagree about the schema.
-- Correcting one is always a new migration, and so is renaming what one
-- created.
--
-- The awkward part is that **roles are cluster-wide and privileges are per
-- database**. One cluster can hold a database that has been through this
-- rename and another that has not, and 0003 will happily recreate the old role
-- for the second one. A first attempt at this file had two exclusive branches
-- -- rename, or create -- and in that situation neither did the right thing:
-- the new database granted everything to the old role while the application
-- asked for the new one.
--
-- So this does not branch on which state it finds. It ensures the role exists,
-- under the new name, and then grants what this database needs to it, every
-- time. Running it twice changes nothing.

-- 1. Take the old name over, where it is still free to.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'redasms_app')
       AND NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'freesms_app') THEN
        ALTER ROLE redasms_app RENAME TO freesms_app;
    END IF;
END
$$;

-- 2. Make sure it is there whatever happened above.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'freesms_app') THEN
        CREATE ROLE freesms_app NOLOGIN;
    END IF;
    EXECUTE format('GRANT freesms_app TO %I', current_user);
END
$$;

-- 3. Grant what this database needs, unconditionally.
--
-- This is the step the first attempt was missing. Privileges do not travel
-- with a rename into a database that never had them.
GRANT USAGE ON SCHEMA public TO freesms_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO freesms_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO freesms_app;

-- schema_migrations is the migrator's bookkeeping. The application has no
-- business reading or writing it, and the blanket grant above would have.
REVOKE ALL ON schema_migrations FROM freesms_app;

-- redasms_app may still exist in a cluster that holds an older database. It is
-- left alone: dropping a role means reassigning what it owns in every database
-- it appears in, which is a great deal of risk for a tidy list.
