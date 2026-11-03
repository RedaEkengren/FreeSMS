-- rollback: safe
-- New columns the previous release neither writes nor reads: its invoices
-- are issued with none, as every invoice before this one was.
--
-- A payment reference with a check digit on every invoice (#234): the
-- invoice number, a length digit and a modulus 10 check digit, as
-- Bankgirot's OCR rules have it, frozen onto the invoice at issue like the
-- seller, so a payment can be matched to the invoice by it. A credit note
-- has none: nothing is paid to it. And the shop's Swish number, frozen
-- with the seller, for the code a customer pays with. Neither is
-- backfilled: an invoice issued without a reference was sent without one,
-- and inventing one now would print something the customer never had.
ALTER TABLE shops
    ADD COLUMN swish_number text CHECK (swish_number IS NULL OR swish_number ~ '^[0-9]{10}$');
ALTER TABLE invoices
    ADD COLUMN payment_ocr         text CHECK (payment_ocr IS NULL OR payment_ocr ~ '^[0-9]{2,25}$'),
    ADD COLUMN seller_swish_number text CHECK (seller_swish_number IS NULL OR seller_swish_number ~ '^[0-9]{10}$');
-- One invoice per reference, so a payment finds the one it pays.
CREATE UNIQUE INDEX invoices_payment_ocr_idx ON invoices (shop_id, payment_ocr) WHERE payment_ocr IS NOT NULL;
