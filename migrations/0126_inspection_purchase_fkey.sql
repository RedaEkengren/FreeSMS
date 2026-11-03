-- rollback: safe
-- A protocol of a purchase is of a purchase that is there (#147). 0066 left
-- the foreign key to the module that brings the table, and 0096 brought
-- vehicle_purchases without it, so a protocol could name a purchase that
-- never existed. ON DELETE RESTRICT, as the rental ends have it (0122):
-- evidence a damage case relies on does not go with what it was about.
-- The previous release writes only purchases that exist, so it runs
-- against this unchanged. delivery_id waits for the deliveries table (#179).
ALTER TABLE inspections ADD CONSTRAINT inspections_purchase_fkey
    FOREIGN KEY (purchase_id) REFERENCES vehicle_purchases(id) ON DELETE RESTRICT;
CREATE INDEX inspections_purchase_idx ON inspections (purchase_id) WHERE purchase_id IS NOT NULL;
