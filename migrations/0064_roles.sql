-- rollback: safe
-- A wider CHECK, a new table and a view, none of which the previous release
-- reads. The previous release reads users.role alone, so a person given a
-- second role here simply has their first one under it; and no person can be
-- given sales, buyer or rental_agent as their first role by this release,
-- because those need the Dealer or the Rental module and neither can be
-- switched on yet (workshop.Available). So no row this release can write
-- holds a role the previous release does not know.
--
-- Roles for car sales and rental (#145), and more than one role per person.
--
-- In a small dealer the salesperson is also the buyer, and in a small rental
-- firm the owner is behind the desk. One role per person forced a choice
-- between giving them too little or making them the owner. A person keeps
-- one main role in users.role -- the one the header names and the home page
-- starts from, and the only one the release before this one reads -- and any
-- others are rows here. A role held is a row, not a list in a column, so
-- that "who can see purchase prices" is a query and not a parse.
ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check
    CHECK (role IN ('owner', 'service_advisor', 'technician', 'parts', 'admin',
                    'sales', 'buyer', 'rental_agent'));

CREATE TABLE user_roles (
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    role       text        NOT NULL CHECK (role IN ('owner', 'service_advisor', 'technician', 'parts', 'admin',
                                                    'sales', 'buyer', 'rental_agent')),
    granted_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    granted_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, role)
);
CREATE INDEX user_roles_shop_idx ON user_roles (shop_id);

ALTER TABLE user_roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_roles FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON user_roles
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON user_roles FROM freesms_app;
GRANT SELECT, INSERT, DELETE ON user_roles TO freesms_app;

-- Every role a person holds: their main one and the others. What the
-- queries that ask "is this person a technician" read, so that a parts
-- person who also turns spanners is offered as a technician.
--
-- security_invoker, so that row level security on the tables underneath
-- applies to whoever reads it rather than to the view's owner.
CREATE VIEW staff_roles WITH (security_invoker = true) AS
SELECT shop_id, id AS user_id, role FROM users
UNION
SELECT shop_id, user_id, role FROM user_roles;
GRANT SELECT ON staff_roles TO freesms_app;
