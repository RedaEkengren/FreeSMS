-- rollback: safe
-- A nullable column the previous release neither reads nor writes, and a
-- partial index that only rows with a hash are in.
--
-- The same photograph put on the same stock car twice -- a double tap on a
-- phone, a retry after a lost connection -- is one photograph (#167). The
-- hash is of the stored, re-encoded image, so what was uploaded twice is
-- what is compared. Rows without one, which is every row before this and
-- every inspection photograph, are never compared.
ALTER TABLE attachments ADD COLUMN content_sha256 bytea CHECK (content_sha256 IS NULL OR length(content_sha256) = 32);
CREATE UNIQUE INDEX attachments_stock_content_idx ON attachments (stock_unit_id, content_sha256)
    WHERE stock_unit_id IS NOT NULL AND content_sha256 IS NOT NULL;
