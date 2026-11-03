-- rollback: safe
-- New columns on Core's invoices and invoice_lines with defaults the
-- previous release never sees differently, and a trigger on the deal's
-- terms, which the previous release never invoices.
--
-- A car sale invoiced (#177). Behind defaults pending the accountant
-- (gate G2, #221): the car sold under the margin scheme is one line at its
-- gross price with no VAT, outside the VAT bands; the invoice carries the
-- statement "Vinstmarginalbeskattning för begagnade varor", frozen in
-- Swedish; the scheme's VAT -- that car's margin times 0.2, never offset --
-- is kept on the invoice for the books and the VAT return and never shown
-- to the buyer, who is never shown the margin.
ALTER TABLE invoice_lines ADD COLUMN margin_scheme boolean NOT NULL DEFAULT false;
ALTER TABLE invoices
    ADD COLUMN margin_scheme_statement text CHECK (margin_scheme_statement IS NULL OR length(margin_scheme_statement) <= 200),
    ADD COLUMN margin_scheme_vat_minor bigint;

-- How a sale is taxed is frozen once it is invoiced (#176): no new terms
-- while an invoice of the sale stands. A credit note releases it.
CREATE FUNCTION vehicle_sale_terms_frozen_by_invoice() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM invoices i
                WHERE i.vehicle_sale_id = NEW.sale_id AND i.credit_of_id IS NULL
                  AND NOT EXISTS (SELECT 1 FROM invoices c WHERE c.credit_of_id = i.id)) THEN
        RAISE EXCEPTION 'the sale has been invoiced, so its terms are frozen; credit the invoice first'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER vehicle_sale_terms_frozen_by_invoice BEFORE INSERT ON vehicle_sale_terms
    FOR EACH ROW EXECUTE FUNCTION vehicle_sale_terms_frozen_by_invoice();
