-- rollback: safe
-- Fills a column 0114 added; the previous release reads terms as before.
--
-- Terms that chose the margin scheme before 0114 recorded which eligibility
-- they relied on have none (#176). They get the assessment that stood when
-- they were written: the newest recorded by then. SaleTaxOfDeal also reads
-- the standing one for any still without, as written by the release before
-- after a rollback.
UPDATE vehicle_sale_terms t
   SET eligibility_id = (SELECT e.id FROM vehicle_sales s
                           JOIN vehicle_purchases p ON p.stock_unit_id = s.stock_unit_id
                           JOIN vat_eligibility e ON e.purchase_id = p.id
                          WHERE s.id = t.sale_id AND e.recorded_at <= t.created_at
                          ORDER BY e.recorded_at DESC, e.id DESC LIMIT 1)
 WHERE t.sale_tax_treatment = 'margin_scheme' AND t.eligibility_id IS NULL;
