-- rollback: safe
-- New tables the previous release neither reads nor writes.
--
-- A reconditioning plan per stock car (#236): an ordered list of steps, each
-- a row, each with its own history of moves, so "how long was it at the
-- painter" is a question the rows answer. Nothing is a counter and nothing
-- is written over: a step's state is its newest event, a step to be redone is
-- an event that reopens it, and a template is replaced by a newer one.

-- What a new plan is made from. The newest template of a shop stands; none
-- means the steps the application starts with.
CREATE TABLE recon_templates (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX recon_templates_idx ON recon_templates (shop_id, created_at DESC);

CREATE TABLE recon_template_steps (
    id          uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    template_id uuid    NOT NULL REFERENCES recon_templates(id) ON DELETE RESTRICT,
    position    integer NOT NULL CHECK (position > 0),
    name        text    NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
    kind        text    NOT NULL CHECK (kind IN ('in_house', 'outside')),
    UNIQUE (template_id, position)
);

-- One car's plan. The price is what was agreed for the step, a cost: it is
-- read only by the roles that may see what a car cost.
CREATE TABLE recon_steps (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id            uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    stock_unit_id      uuid        NOT NULL REFERENCES stock_units(id) ON DELETE RESTRICT,
    position           integer     NOT NULL CHECK (position > 0),
    name               text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
    kind               text        NOT NULL CHECK (kind IN ('in_house', 'outside')),
    -- Who holds the car for it: a person in the shop, or the supplier's name.
    owner              text        CHECK (owner IS NULL OR length(owner) <= 200),
    due_on             date,
    agreed_price_minor bigint      CHECK (agreed_price_minor IS NULL OR agreed_price_minor BETWEEN 0 AND 100000000000),
    created_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by         uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (stock_unit_id, position)
);
CREATE INDEX recon_steps_unit_idx ON recon_steps (stock_unit_id, position);

-- What happened to a step. No event is a step waiting its turn; the newest
-- says where it is now.
CREATE TABLE recon_step_events (
    id      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    step_id uuid        NOT NULL REFERENCES recon_steps(id) ON DELETE RESTRICT,
    seq     bigint      GENERATED ALWAYS AS IDENTITY,
    kind    text        NOT NULL CHECK (kind IN ('started', 'done', 'skipped', 'reopened')),
    note    text        CHECK (note IS NULL OR length(note) <= 300),
    at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX recon_step_events_idx ON recon_step_events (step_id, seq);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['recon_templates', 'recon_template_steps', 'recon_steps', 'recon_step_events'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END;
$$;
