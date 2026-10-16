-- rollback: safe
-- New tables the previous release never reads, and one nullable column on the
-- stock ledger it never writes or reads.
--
-- A shop that starts using FreeSMS has a customer list, a vehicle register and
-- a shelf of parts somewhere else. An import is not a loop of ordinary saves:
-- it is a reviewed batch, previewed before anything is written, identified by
-- the file it came from so that a second upload of the same file is found and
-- not repeated, and every record it created or filled in knows which batch it
-- came from.
--
-- A file's rows are kept only while the batch waits to be applied: they hold
-- what the file said, personal data among it, and are wiped when the batch is
-- applied, discarded or left for a week. What stays is the batch's own record:
-- which file, who, when, what it came to.
CREATE TABLE import_batches (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    kind         text        NOT NULL CHECK (kind IN ('customers', 'vehicles', 'parts', 'stock')),
    file_name    text        NOT NULL CHECK (length(file_name) <= 200),
    -- The file's own fingerprint, so the same file is the same batch.
    file_sha256  bytea       NOT NULL,
    encoding     text        NOT NULL,
    status       text        NOT NULL DEFAULT 'previewed' CHECK (status IN ('previewed', 'applied', 'discarded')),
    row_count    integer     NOT NULL,
    created_count integer    NOT NULL DEFAULT 0,
    updated_count integer    NOT NULL DEFAULT 0,
    skipped_count integer    NOT NULL DEFAULT 0,
    failed_count  integer    NOT NULL DEFAULT 0,
    created_by   uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at   timestamptz NOT NULL DEFAULT clock_timestamp(),
    applied_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    applied_at   timestamptz,
    CHECK ((status = 'applied') = (applied_at IS NOT NULL))
);
-- A file is one batch while it is waiting or done; one that was discarded may
-- be uploaded again.
CREATE UNIQUE INDEX import_batches_file_idx ON import_batches (shop_id, kind, file_sha256) WHERE status <> 'discarded';

CREATE TABLE import_rows (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    batch_id     uuid        NOT NULL REFERENCES import_batches(id) ON DELETE RESTRICT,
    row_no       integer     NOT NULL,
    ref          text,
    -- What the file said, normalised; and what the preview made of it. Both
    -- wiped when the batch is done with.
    data         jsonb,
    info         jsonb,
    action       text        NOT NULL CHECK (action IN ('create', 'update', 'unchanged', 'error')),
    -- The person's choice for a row that may be somebody already known.
    decision     text        CHECK (decision IN ('create', 'link', 'skip')),
    decision_target text,
    outcome      text        CHECK (outcome IN ('created', 'updated', 'unchanged', 'skipped', 'linked', 'failed')),
    entity_id    uuid,
    message      text        CHECK (message IS NULL OR length(message) <= 500),
    UNIQUE (batch_id, row_no)
);

-- What a source system called a record, and which record that became. A
-- later file naming the same reference finds the record instead of making
-- another.
CREATE TABLE import_refs (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id      uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    kind         text        NOT NULL CHECK (kind IN ('customers', 'vehicles', 'parts', 'stock')),
    external_id  text        NOT NULL CHECK (length(external_id) BETWEEN 1 AND 200),
    entity_id    uuid        NOT NULL,
    batch_id     uuid        NOT NULL REFERENCES import_batches(id) ON DELETE RESTRICT,
    UNIQUE (shop_id, kind, external_id)
);

-- Where a shelf's opening balance came from.
ALTER TABLE stock_movements ADD COLUMN import_batch_id uuid REFERENCES import_batches(id) ON DELETE RESTRICT;

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['import_batches', 'import_rows', 'import_refs'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END $$;
GRANT UPDATE (status, created_count, updated_count, skipped_count, failed_count, applied_by, applied_at) ON import_batches TO freesms_app;
GRANT UPDATE (data, info, decision, decision_target, outcome, entity_id, message) ON import_rows TO freesms_app;
