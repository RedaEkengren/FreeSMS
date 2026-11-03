-- rollback: safe
-- A new table, a foreign key on a column the previous release never sets,
-- and one more source an odometer reading may have, which it never writes.
--
-- Handing a sold car over (#179) is a recorded event, once per sale: its
-- moment is the delivery the Consumer Sales Act counts from -- two years
-- in which a defect is presumed to have been there, three in which the
-- dealer answers for it -- and is frozen here, never typed again. A
-- delivery made before every condition was met says why, as the owner's
-- decision.
CREATE TABLE vehicle_sale_deliveries (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id             uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    sale_id             uuid        NOT NULL UNIQUE REFERENCES vehicle_sales(id) ON DELETE RESTRICT,
    delivered_at        timestamptz NOT NULL,
    -- Handed over at the buyer's address: the moment is the handover there,
    -- not the leaving of the yard.
    at_home             boolean     NOT NULL DEFAULT false,
    odometer_reading_id uuid        REFERENCES odometer_readings(id) ON DELETE RESTRICT,
    override_reason     text        CHECK (override_reason IS NULL OR length(override_reason) BETWEEN 1 AND 300),
    recorded_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    recorded_by         uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK (delivered_at <= recorded_at + interval '1 hour')
);

ALTER TABLE vehicle_sale_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE vehicle_sale_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON vehicle_sale_deliveries
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON vehicle_sale_deliveries FROM freesms_app;
GRANT SELECT, INSERT ON vehicle_sale_deliveries TO freesms_app;
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON vehicle_sale_deliveries
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');

-- A delivery protocol is of the sale it hands over (0066 left the key to
-- the module that brings the table; #147).
ALTER TABLE inspections ADD CONSTRAINT inspections_delivery_fkey
    FOREIGN KEY (delivery_id) REFERENCES vehicle_sales(id) ON DELETE RESTRICT;
CREATE INDEX inspections_delivery_idx ON inspections (delivery_id) WHERE delivery_id IS NOT NULL;

SELECT widen_text_check('odometer_readings', 'odometer_readings_source_check', 'source', ARRAY['delivery']);
