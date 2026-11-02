-- rollback: safe
-- A new column, filled by a trigger whatever release writes the terms, and
-- the margin scheme refused for a car not eligible -- which the previous
-- release already refuses in its own code, so it writes nothing this
-- refuses.
--
-- How a sale is taxed (#176) is chosen on the deal's terms; choosing the
-- margin scheme relies on the car's VAT eligibility as it stood then (#161),
-- and the terms say which assessment that was. Who chose it and when are
-- the terms version's created_by and created_at. A car with no purchase --
-- consigned, from the fleet, imported with no evidence -- or one assessed
-- not eligible cannot be sold under the scheme, refused here as well as in
-- the application.
ALTER TABLE vehicle_sale_terms ADD COLUMN eligibility_id uuid REFERENCES vat_eligibility(id) ON DELETE RESTRICT;

-- Terms that chose the scheme before this: the assessment that stood when
-- they were written -- the newest recorded by then.
UPDATE vehicle_sale_terms t
   SET eligibility_id = (SELECT e.id FROM vehicle_sales s
                           JOIN vehicle_purchases p ON p.stock_unit_id = s.stock_unit_id
                           JOIN vat_eligibility e ON e.purchase_id = p.id
                          WHERE s.id = t.sale_id AND e.recorded_at <= t.created_at
                          ORDER BY e.recorded_at DESC, e.id DESC LIMIT 1)
 WHERE t.sale_tax_treatment = 'margin_scheme' AND t.eligibility_id IS NULL;

CREATE FUNCTION vehicle_sale_terms_eligibility() RETURNS trigger AS $$
DECLARE
    standing uuid;
    ok boolean;
BEGIN
    IF NEW.sale_tax_treatment IS DISTINCT FROM 'margin_scheme' THEN
        NEW.eligibility_id := NULL;
        RETURN NEW;
    END IF;
    SELECT e.id, e.eligible INTO standing, ok
      FROM vehicle_sales s
      JOIN vehicle_purchases p ON p.stock_unit_id = s.stock_unit_id
      JOIN vat_eligibility e ON e.purchase_id = p.id
     WHERE s.id = NEW.sale_id
       AND NOT EXISTS (SELECT 1 FROM vat_eligibility n WHERE n.supersedes_id = e.id);
    IF standing IS NULL OR NOT ok THEN
        RAISE EXCEPTION 'this car may not be sold under the margin scheme: its purchase does not make it eligible'
            USING ERRCODE = 'check_violation';
    END IF;
    NEW.eligibility_id := standing;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER vehicle_sale_terms_eligibility BEFORE INSERT ON vehicle_sale_terms
    FOR EACH ROW EXECUTE FUNCTION vehicle_sale_terms_eligibility();
