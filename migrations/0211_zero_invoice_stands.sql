-- rollback: safe
-- A function replaced with the same signature; the previous release never
-- invoices a sale.
--
-- The invoice that stands for a car sale (0209), read again: issued, and
-- not credited in full -- credited in full meaning it has a credit note
-- and what it came to less what its credit notes took back is nothing or
-- less. A 0 kr invoice, with no credit note, stands; the application
-- invoices no sale of nothing.
CREATE OR REPLACE FUNCTION sale_invoice_standing(sale uuid) RETURNS uuid LANGUAGE sql STABLE AS $$
    SELECT i.id FROM invoices i
     WHERE i.vehicle_sale_id = sale AND i.credit_of_id IS NULL
       AND NOT (EXISTS (SELECT 1 FROM invoices c WHERE c.credit_of_id = i.id)
                AND i.gross_minor + (SELECT sum(c.gross_minor) FROM invoices c WHERE c.credit_of_id = i.id) <= 0)
     ORDER BY i.number DESC LIMIT 1
$$;
