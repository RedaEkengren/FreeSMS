-- rollback: safe
-- A new trigger, no table or column. The previous release lets a cash
-- invoice's payment be reversed, which is what is refused here; every
-- other row it writes is accepted.
--
-- A cash invoice (0085) is frozen as paid: the document says it was paid
-- on issue and how, and that is checked once, as it is issued. A payment
-- it lists reversed afterwards -- "recorded in error" -- left a document
-- saying Paid, and asking nobody to pay, on an invoice that owed its whole
-- amount again (review round five). Such a payment is part of the
-- document, and like the document it stands: a mistake is corrected with
-- a credit note, and a new invoice if one is needed. Money given back on a
-- cash invoice is a refund, which 0086 holds within what the credit note
-- makes owed back.
CREATE FUNCTION cash_invoice_payment_stands() RETURNS trigger AS $$
BEGIN
    IF NEW.reverses_id IS NOT NULL AND NEW.invoice_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM cash_invoices c
         WHERE c.invoice_id = NEW.invoice_id
           AND c.paid_with @> jsonb_build_array(jsonb_build_object('payment_id', NEW.reverses_id::text))) THEN
        RAISE EXCEPTION 'a cash invoice''s payment is not reversed; credit the invoice'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER cash_invoice_payment_stands BEFORE INSERT ON invoice_payments
    FOR EACH ROW EXECUTE FUNCTION cash_invoice_payment_stands();
