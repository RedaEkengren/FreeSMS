-- Not losing what somebody typed.
--
-- The most damning review in the whole category, about a paid competitor:
--
--   the system randomly glitches and shuts down and when it does if you
--   haven't saved your 3-5 quotes or orders all those hours of work is all
--   lost
--
-- That has to be impossible here by construction rather than unlikely.

-- What a request already did, so doing it again does it once.
--
-- A flaky connection means the same request arrives twice: the phone gave up
-- waiting, the technician pressed again, the offline queue replayed. Without
-- this each one opens a second job.
CREATE TABLE idempotency_keys (
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE CASCADE,
    key          text        NOT NULL CHECK (length(key) BETWEEN 16 AND 128),

    -- What the request was. A key reused for a different request is a client
    -- bug, and returning the first answer to the second question would be
    -- worse than refusing.
    request_hash bytea       NOT NULL,

    status       integer     NOT NULL,
    location     text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (shop_id, key)
);
CREATE INDEX idempotency_keys_age_idx ON idempotency_keys (created_at);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON idempotency_keys
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- What somebody has typed and not yet submitted.
--
-- Kept on the server as well as in the browser, because a phone that is lost,
-- wiped or swapped takes its local storage with it. One draft per person per
-- form: a second technician typing on the same form has their own.
CREATE TABLE drafts (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- Which form, and which thing it is about: "job-lines:<uuid>".
    form       text        NOT NULL CHECK (length(trim(form)) > 0),
    fields     jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, form)
);
CREATE INDEX drafts_age_idx ON drafts (updated_at);

ALTER TABLE drafts ENABLE ROW LEVEL SECURITY;
ALTER TABLE drafts FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON drafts
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys, drafts TO redasms_app;
