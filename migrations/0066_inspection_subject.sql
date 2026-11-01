-- rollback: safe
-- New columns with defaults, a CHECK every existing row meets, a NOT NULL
-- relaxed under it, and triggers that act only on protocols that are not a
-- job's. The previous release reads inspections.work_order_id as always
-- set, and it is for every row this release can write: a purchase, a
-- check-out, a check-in or a delivery belongs to car sales or rental, which
-- no release offers yet (workshop.Available), and their operations refuse a
-- module that is off. Existing templates are the workshop's by default, and
-- the release before goes on making templates the same way.
--
-- An inspection has a subject (#147): what it is evidence for. The
-- workshop's job, a car bought in (purchase), a rental handed over
-- (check_out) or taken back (check_in), a car handed to its buyer
-- (delivery). The templates, the items, the photographs are one machine for
-- all of them; every protocol names its vehicle, so two of the same vehicle
-- can be put side by side.
--
-- Every inspection there is was a job's, and reads so through the default,
-- written to no row.
ALTER TABLE inspections
    ADD COLUMN subject text NOT NULL DEFAULT 'work_order'
        CHECK (subject IN ('work_order', 'purchase', 'check_out', 'check_in', 'delivery')),
    -- One reference per subject. Their tables come with their modules
    -- (purchases with #160, rentals with #189, sales with #172); their
    -- migrations add the foreign keys, ON DELETE RESTRICT: evidence a
    -- damage case relies on does not go with what it was about.
    ADD COLUMN purchase_id uuid,
    ADD COLUMN check_out_id uuid,
    ADD COLUMN check_in_id uuid,
    ADD COLUMN delivery_id uuid,
    -- The vehicle it is of. Read through the job for the workshop's, which
    -- keep doing so; required for every other subject.
    ADD COLUMN vehicle_id uuid REFERENCES vehicles(id) ON DELETE RESTRICT,
    -- A completed protocol is not edited; a correction is a new one naming
    -- the one it supersedes.
    ADD COLUMN supersedes_id uuid REFERENCES inspections(id) ON DELETE RESTRICT,
    -- Started and left: kept, said so, and frozen like a completed one.
    ADD COLUMN abandoned_at timestamptz,
    ADD COLUMN abandoned_reason text CHECK (abandoned_reason IS NULL OR length(abandoned_reason) <= 300),
    -- The phone's own key for the request that started it, so a replay of a
    -- check-out done offline finds the protocol it already started.
    ADD COLUMN request_key text CHECK (request_key IS NULL OR length(request_key) BETWEEN 1 AND 100);

ALTER TABLE inspections ADD CONSTRAINT inspections_subject_reference CHECK (
    (subject = 'work_order') = (work_order_id IS NOT NULL)
    AND (subject = 'purchase') = (purchase_id IS NOT NULL)
    AND (subject = 'check_out') = (check_out_id IS NOT NULL)
    AND (subject = 'check_in') = (check_in_id IS NOT NULL)
    AND (subject = 'delivery') = (delivery_id IS NOT NULL)
    AND (subject = 'work_order' OR vehicle_id IS NOT NULL)
    AND NOT (completed_at IS NOT NULL AND abandoned_at IS NOT NULL)
);
-- The workshop's keep ON DELETE CASCADE from their job, as they always
-- have: a job is never deleted, and changing it would alter a constraint
-- the release before relies on for nothing.
ALTER TABLE inspections ALTER COLUMN work_order_id DROP NOT NULL;

CREATE UNIQUE INDEX inspections_request_key ON inspections (shop_id, request_key) WHERE request_key IS NOT NULL;
CREATE INDEX inspections_vehicle_idx ON inspections (vehicle_id) WHERE vehicle_id IS NOT NULL;

-- What a template may be used for. The workshop's, by default and for every
-- template there is. Its labels are the shop's own words and are never
-- translated.
ALTER TABLE inspection_templates
    ADD COLUMN subjects text[] NOT NULL DEFAULT '{work_order}'
        CHECK (cardinality(subjects) > 0
               AND subjects <@ ARRAY['work_order', 'purchase', 'check_out', 'check_in', 'delivery']);

-- A key per item, so a check-out and the check-in after it can be put side
-- by side item by item (#194). The label as the checklist is saved, in lower
-- case -- the same rule that refuses a checkpoint twice on one list -- so two
-- templates with the same checkpoints share their keys. Copied onto the
-- protocol with the label. Templates saved before have none until saved
-- again, and their protocols compare by nothing, which is honest.
ALTER TABLE inspection_template_items ADD COLUMN item_key text
    CHECK (item_key IS NULL OR length(item_key) BETWEEN 1 AND 200);
CREATE UNIQUE INDEX inspection_template_items_key ON inspection_template_items (template_id, item_key)
    WHERE item_key IS NOT NULL;
ALTER TABLE inspection_items ADD COLUMN item_key text;

-- A protocol that is not a job's is frozen once completed or abandoned: its
-- items, its answers and its photographs. The workshop's are not, here: the
-- workshop has always let a note or a photograph follow the end of the
-- check, and changing that is a decision for the workshop, not a side
-- effect of this one.
CREATE FUNCTION inspection_is_frozen(inspection uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM inspections i
                   WHERE i.id = inspection AND i.subject <> 'work_order'
                     AND (i.completed_at IS NOT NULL OR i.abandoned_at IS NOT NULL));
$$;

CREATE FUNCTION protocols_are_frozen() RETURNS trigger AS $$
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

CREATE TRIGGER protocols_are_frozen BEFORE UPDATE OR DELETE ON inspections
    FOR EACH ROW EXECUTE FUNCTION protocols_are_frozen();
CREATE TRIGGER protocols_are_frozen BEFORE INSERT OR UPDATE OR DELETE ON inspection_items
    FOR EACH ROW EXECUTE FUNCTION protocols_are_frozen();
-- An attachment of anything else finds no item, and passes.
CREATE TRIGGER protocols_are_frozen BEFORE INSERT OR UPDATE OR DELETE ON attachments
    FOR EACH ROW EXECUTE FUNCTION protocols_are_frozen();

-- A customer's yes or no, and the link they give it on, are for a job's
-- findings: a purchase appraisal or a check-in has nobody to approve a
-- price, and a decision on one would become a priced line (0026).
CREATE FUNCTION decisions_are_the_workshops() RETURNS trigger AS $$
DECLARE
    s text;
BEGIN
    IF TG_TABLE_NAME = 'inspection_decisions' THEN
        SELECT i.subject INTO s FROM inspection_items it JOIN inspections i ON i.id = it.inspection_id
         WHERE it.id = NEW.item_id;
    ELSE -- inspection_shares
        SELECT i.subject INTO s FROM inspections i WHERE i.id = NEW.inspection_id;
    END IF;
    IF s IS DISTINCT FROM 'work_order' THEN
        RAISE EXCEPTION 'only a job''s inspection is put to the customer (subject %)', coalesce(s, '?')
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER decisions_are_the_workshops BEFORE INSERT ON inspection_decisions
    FOR EACH ROW EXECUTE FUNCTION decisions_are_the_workshops();
CREATE TRIGGER decisions_are_the_workshops BEFORE INSERT ON inspection_shares
    FOR EACH ROW EXECUTE FUNCTION decisions_are_the_workshops();
