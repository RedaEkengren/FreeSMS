-- rollback: safe
-- What an electronic invoice needs that a paper one does not, kept while it can
-- still be kept.
--
-- An issued invoice is frozen, so a fact that was not written onto it when it
-- was issued can never be added. A paper invoice gets by with one address line
-- and a payment reference in free text; an EN 16931 invoice (Peppol BIS Billing
-- 3.0, the Swedish and EU standard) wants the postal code, town and country as
-- separate facts, an electronic address for each party, the buyer's reference,
-- a unit on every line and a VAT category on every line. Nothing here builds
-- the export: it only makes sure that no invoice issued from now on is shut out
-- of it.
--
-- Everything is nullable and nothing is backfilled, for the reason 0021 gave:
-- an invoice issued without these has no honest value to print, and inventing
-- one from today's shop or customer row would be a guess shown as a fact. The
-- previous release ignores every column below, which is what makes this safe.

-- 1. What the shop and the customer are asked for.
ALTER TABLE shops
    -- The shop's address on the Peppol network, usually its company number.
    ADD COLUMN endpoint_id    text,
    -- What the free-text payment reference is. A bankgiro and an IBAN are
    -- different payment means in an e-invoice, and the number does not say.
    ADD COLUMN payment_scheme text;
ALTER TABLE shops
    ADD CONSTRAINT shops_payment_scheme_check
    CHECK (payment_scheme IS NULL OR payment_scheme IN ('bankgiro', 'plusgiro', 'iban'));

ALTER TABLE customers
    -- Where the buyer receives electronic invoices.
    ADD COLUMN endpoint_id     text,
    -- What the buyer asks to have printed so the invoice reaches the right
    -- desk ("Er referens"). Public buyers refuse an invoice without one.
    ADD COLUMN buyer_reference text;

-- 2. The same facts frozen onto the document, beside what is frozen already.
ALTER TABLE invoices
    ADD COLUMN seller_street         text,
    ADD COLUMN seller_postal_code    text,
    ADD COLUMN seller_city           text,
    ADD COLUMN seller_country        text,
    ADD COLUMN seller_endpoint_id    text,
    ADD COLUMN seller_payment_scheme text,
    ADD COLUMN customer_postal_code  text,
    ADD COLUMN customer_city         text,
    ADD COLUMN customer_country      text,
    ADD COLUMN customer_endpoint_id  text,
    ADD COLUMN customer_reference    text;

-- 3. And on every line: the unit (UN/ECE Recommendation 20 code, such as HUR
-- for an hour and C62 for "one") and the VAT category (UNCL5305: S for a
-- standard rate). A line at 0 per cent has no category yet, because 0 per cent
-- is exempt, zero-rated, reverse charge or outside the scope of VAT, and which
-- one is a decision about the sale that a rate cannot make; the column stays
-- empty there rather than guess.
ALTER TABLE invoice_lines
    ADD COLUMN unit_code    text,
    ADD COLUMN vat_category text;
