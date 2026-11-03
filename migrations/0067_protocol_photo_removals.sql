-- rollback: safe
-- A new table and a trigger function that refuses exactly what it refused
-- before, unless a row in that new table says otherwise. The previous release
-- never writes one, so for it a finished protocol's photograph still cannot
-- be deleted -- as it could not before -- and nothing it reads has changed.
--
-- A photograph on a finished protocol can be removed when it is personal
-- data (0066 froze the protocol, attachments included). A check-in photo
-- catches what it was not aimed at: a bystander, another car's plate. The
-- file is the personal data, and it has to be possible to delete it.
--
-- The protocol is still evidence, so the removal is not silent. Each one
-- leaves a row here, on the protocol, saying that a photograph was removed
-- from which checkpoint, when, by whom and why. A damage case that relied on
-- a check-in can then see that the protocol had a photograph it no longer
-- has, rather than finding one fewer and no reason.
CREATE TABLE protocol_photo_removals (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id            uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    inspection_id      uuid        NOT NULL REFERENCES inspections(id) ON DELETE RESTRICT,
    inspection_item_id uuid        NOT NULL REFERENCES inspection_items(id) ON DELETE RESTRICT,
    -- The attachment's own id, kept after the row has gone: no foreign key,
    -- since the point is that the row it named is deleted.
    attachment_id      uuid        NOT NULL UNIQUE,
    removed_at         timestamptz NOT NULL DEFAULT now(),
    removed_by         uuid        REFERENCES users(id) ON DELETE RESTRICT,
    reason             text        NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 300)
);
CREATE INDEX protocol_photo_removals_inspection_idx ON protocol_photo_removals (inspection_id);

ALTER TABLE protocol_photo_removals ENABLE ROW LEVEL SECURITY;
ALTER TABLE protocol_photo_removals FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON protocol_photo_removals
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- Written once and never changed: what was removed is a record, like the
-- erasure it often is part of.
GRANT SELECT, INSERT ON protocol_photo_removals TO freesms_app;

-- As 0066, and one thing more: an attachment on a finished protocol may be
-- deleted when its removal has been recorded in the same transaction.
CREATE OR REPLACE FUNCTION protocols_are_frozen() RETURNS trigger AS $$
DECLARE
    inspection uuid;
BEGIN
    IF TG_TABLE_NAME = 'inspections' THEN
        IF OLD.subject <> 'work_order' AND (OLD.completed_at IS NOT NULL OR OLD.abandoned_at IS NOT NULL) THEN
            RAISE EXCEPTION 'a finished protocol cannot be changed (%); start a new one that supersedes it', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    END IF;
    IF TG_TABLE_NAME = 'inspection_items' THEN
        inspection := CASE WHEN TG_OP = 'INSERT' THEN NEW.inspection_id ELSE OLD.inspection_id END;
    ELSE -- attachments
        IF TG_OP = 'DELETE' AND EXISTS (SELECT 1 FROM protocol_photo_removals r WHERE r.attachment_id = OLD.id) THEN
            RETURN OLD;
        END IF;
        SELECT it.inspection_id INTO inspection FROM inspection_items it
         WHERE it.id = CASE WHEN TG_OP = 'INSERT' THEN NEW.inspection_item_id ELSE OLD.inspection_item_id END;
    END IF;
    IF inspection IS NOT NULL AND inspection_is_frozen(inspection) THEN
        RAISE EXCEPTION 'a finished protocol cannot be changed (%); start a new one that supersedes it', inspection
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
END;
$$ LANGUAGE plpgsql;
