-- Authentication: sessions, and the record of failed attempts.
--
-- One deliberate exception to FORCE row level security, made here rather than
-- left implicit.
--
-- Something has to decide which shop a request belongs to before any scope
-- exists -- that is the chicken and egg of multi-tenancy. Rather than punching
-- a hole in a policy for the authentication path, shops stops being FORCEd:
-- the schema owner may read the tenant directory, the application role still
-- may not see a shop other than its own. Resolution happens as the owner,
-- before login; everything afterwards runs scoped, including the lookup of the
-- user and the creation of the session.
--
-- The line is: FORCE wherever customer data lives. The directory of shops is
-- not customer data, and one table readable by the role that owns the schema
-- is a smaller hole than a policy with an exception in it.
ALTER TABLE shops NO FORCE ROW LEVEL SECURITY;

-- A session is a row, not a signed cookie.
--
-- That is what makes "a deactivated user's session dies immediately" true
-- rather than aspirational: a self-contained token stays valid until it
-- expires no matter what happens to the account, because nothing is consulted
-- when it is presented. A row can be deleted, and the user's active flag is
-- checked on every request.
--
-- The token itself is never stored. What is stored is its SHA-256, so a stolen
-- database backup does not hand over live sessions. SHA-256 rather than a
-- password hash is right here: the token is 256 bits of randomness, so there
-- is nothing to brute force, and this runs on every single request.
CREATE TABLE sessions (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    user_id      uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_sha256 bytea       NOT NULL UNIQUE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    -- Absolute expiry. Idle expiry is applied on top, in the application,
    -- against last_seen_at.
    expires_at   timestamptz NOT NULL,
    user_agent   text,
    CHECK (expires_at > created_at)
);
CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expiry_idx ON sessions (expires_at);

ALTER TABLE sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON sessions
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- Failed sign-in attempts, so that guessing a password is slow.
--
-- Recorded per shop and address rather than per account. Locking an account on
-- failed attempts lets anyone lock a colleague out by typing the wrong
-- password five times, which turns a login form into a denial of service
-- against a named person.
CREATE TABLE login_attempts (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE CASCADE,
    email       citext      NOT NULL,
    remote_ip   inet,
    attempted_at timestamptz NOT NULL DEFAULT now(),
    succeeded   boolean     NOT NULL
);
CREATE INDEX login_attempts_window_idx ON login_attempts (shop_id, email, attempted_at DESC);

ALTER TABLE login_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE login_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON login_attempts
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON sessions, login_attempts TO redasms_app;
