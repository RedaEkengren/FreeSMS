-- Digital vehicle inspections.
--
-- The technician photographs what they found, the customer sees the evidence
-- and says yes or no to each item. It is the feature that raises order value,
-- and it is the one place this can be better than the Swedish desktop systems
-- rather than catching up with them: their software cannot photograph a brake
-- disc.

-- What gets checked, per kind of job. Editable by the shop: a template that
-- cannot be changed is a template that gets ignored and replaced by a habit.
CREATE TABLE inspection_templates (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    name       text        NOT NULL CHECK (length(trim(name)) > 0),
    active     boolean     NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX inspection_templates_shop_idx ON inspection_templates (shop_id) WHERE active;

CREATE TABLE inspection_template_items (
    id          uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    template_id uuid    NOT NULL REFERENCES inspection_templates(id) ON DELETE CASCADE,
    position    integer NOT NULL,
    label       text    NOT NULL CHECK (length(trim(label)) > 0),
    UNIQUE (template_id, position)
);

-- One run of a template against one job.
--
-- The labels are copied from the template rather than joined, for the same
-- reason an invoice copies its lines: editing a template next month must not
-- change what a customer was shown last month.
CREATE TABLE inspections (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    work_order_id uuid        NOT NULL REFERENCES work_orders(id) ON DELETE CASCADE,
    template_id   uuid        REFERENCES inspection_templates(id) ON DELETE SET NULL,
    template_name text        NOT NULL,
    performed_by  uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    started_at    timestamptz NOT NULL DEFAULT now(),

    -- An inspection with nothing wrong is still evidence that the check was
    -- done, which is exactly what a customer disputing a later failure asks
    -- for. Completed with no findings is a result, not an empty record.
    completed_at  timestamptz
);
CREATE INDEX inspections_order_idx ON inspections (work_order_id);

CREATE TABLE inspection_items (
    id            uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    inspection_id uuid    NOT NULL REFERENCES inspections(id) ON DELETE CASCADE,
    position      integer NOT NULL,
    label         text    NOT NULL,
    status        text    CHECK (status IN ('pass', 'attention', 'fail')),
    note          text,
    UNIQUE (inspection_id, position)
);
CREATE INDEX inspection_items_inspection_idx ON inspection_items (inspection_id);

-- Photographs belong to an item. attachments already existed for the work
-- order; this widens it rather than adding a second place files live.
ALTER TABLE attachments ADD COLUMN inspection_item_id uuid
    REFERENCES inspection_items(id) ON DELETE RESTRICT;
CREATE INDEX attachments_item_idx ON attachments (inspection_item_id)
    WHERE inspection_item_id IS NOT NULL;

ALTER TABLE attachments DROP CONSTRAINT attachments_check;
ALTER TABLE attachments ADD CONSTRAINT attachments_belong_somewhere
    CHECK (work_order_id IS NOT NULL OR vehicle_id IS NOT NULL OR inspection_item_id IS NOT NULL);

-- The link the customer is sent.
--
-- Treated as public, because it will be. Customers forward these to a partner,
-- to a workplace, into a group chat. So: one inspection only, expiring,
-- revocable, and carrying nothing the customer does not already know. The
-- token is stored as its SHA-256 for the same reason a session token is --
-- a leaked backup must not hand over live links.
CREATE TABLE inspection_shares (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    inspection_id uuid        NOT NULL REFERENCES inspections(id) ON DELETE CASCADE,
    token_sha256  bytea       NOT NULL UNIQUE,
    created_by    uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    CHECK (expires_at > created_at)
);
CREATE INDEX inspection_shares_inspection_idx ON inspection_shares (inspection_id);

-- What the customer said, appended.
--
-- Never updated. A customer who approves and then declines ten minutes later
-- has done two things, and which one is current is a question about order, not
-- a reason to lose the first. It is also the record of who said yes when
-- somebody else turns up to collect the car.
CREATE TABLE inspection_decisions (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    item_id    uuid        NOT NULL REFERENCES inspection_items(id) ON DELETE CASCADE,
    decision   text        NOT NULL CHECK (decision IN ('approved', 'declined')),
    decided_at timestamptz NOT NULL DEFAULT now(),

    -- Which link it came through, or NULL when somebody said it at the
    -- counter. "Approved over the telephone by the person who dropped it off"
    -- and "approved from the link" are different things to be able to show.
    share_id   uuid        REFERENCES inspection_shares(id) ON DELETE SET NULL,
    decided_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (share_id IS NOT NULL OR decided_by IS NOT NULL)
);
CREATE INDEX inspection_decisions_item_idx ON inspection_decisions (item_id, decided_at DESC);

DO $$
DECLARE t text;
BEGIN
    FOR t IN SELECT unnest(ARRAY['inspection_templates', 'inspection_template_items',
                                 'inspections', 'inspection_items',
                                 'inspection_shares', 'inspection_decisions'])
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$
            CREATE POLICY shop_isolation ON %I
                USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
                WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
        $p$, t);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO redasms_app', t);
    END LOOP;
END
$$;
