-- rollback: safe
-- A function, and a trigger function replaced with the same name; the
-- previous release never invoices a sale.
--
-- The invoice that stands for a car sale (#177): one of the sale's that is
-- not a credit note and is not credited in full -- its gross less what its
-- credit notes took back is above nothing, as a credit is bounded
-- (planCreditTx). A credit of part of it leaves it standing. One rule, read
-- by the freeze on the sale's terms and by the application alike.
CREATE FUNCTION sale_invoice_standing(sale uuid) RETURNS uuid LANGUAGE sql STABLE AS $$
    SELECT i.id FROM invoices i
     WHERE i.vehicle_sale_id = sale AND i.credit_of_id IS NULL
       AND i.gross_minor + coalesce((SELECT sum(c.gross_minor) FROM invoices c WHERE c.credit_of_id = i.id), 0) > 0
     ORDER BY i.number DESC LIMIT 1
$$;

CREATE OR REPLACE FUNCTION vehicle_sale_terms_frozen_by_invoice() RETURNS trigger AS $$
BEGIN
    IF sale_invoice_standing(NEW.sale_id) IS NOT NULL THEN
        RAISE EXCEPTION 'the sale has been invoiced, so its terms are frozen; credit the invoice first'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
