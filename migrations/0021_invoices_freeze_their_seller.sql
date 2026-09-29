-- An invoice froze the buyer and read the seller live.
--
-- customer_name, customer_address, customer_org_number and customer_vat_number
-- have been snapshotted on the document since invoicing was built. The seller
-- was a join to shops, so a workshop that changed its name or moved premises
-- silently rewrote every invoice it had ever sent. That is exactly what the
-- buyer columns exist to prevent.
--
-- It is also a legal problem before it is a correctness one: a Swedish invoice
-- must carry the seller's name, address and VAT registration number, and none
-- of those existed anywhere in the schema.

-- 1. What a seller is. These belong to the shop and change over its life.
ALTER TABLE shops
    ADD COLUMN address_line1 text,
    ADD COLUMN address_line2 text,
    ADD COLUMN postal_code   text,
    ADD COLUMN city          text,
    ADD COLUMN country       text NOT NULL DEFAULT 'SE',
    ADD COLUMN org_number    text,
    ADD COLUMN vat_number    text,
    ADD COLUMN phone         text,
    ADD COLUMN email         text,
    -- Bankgiro, plusgiro or IBAN. One free-text field rather than three
    -- columns: which one a shop uses is its own business, and the invoice
    -- prints whatever it was told.
    ADD COLUMN payment_reference text,
    -- Days, not a date. "30 dagar netto" printed in January must not become a
    -- different due date when the document is opened in March.
    ADD COLUMN payment_terms_days integer NOT NULL DEFAULT 30,
    -- Swedish practice prints this, and a shop that is not approved must not
    -- claim to be.
    ADD COLUMN f_tax boolean NOT NULL DEFAULT false;

ALTER TABLE shops
    ADD CONSTRAINT shops_payment_terms_days_check CHECK (payment_terms_days >= 0);

-- 2. The same fields frozen onto the document, mirroring the buyer.
--
-- Nullable, and deliberately not backfilled. Every invoice issued before this
-- migration was issued without a seller block, and inventing one from today's
-- shops row would be a guess printed as a fact on a document that is supposed
-- to be immutable. An empty seller on an old invoice is the honest answer, and
-- there are none in production because nothing is deployed yet.
ALTER TABLE invoices
    ADD COLUMN seller_name              text,
    ADD COLUMN seller_address           text,
    ADD COLUMN seller_org_number        text,
    ADD COLUMN seller_vat_number        text,
    ADD COLUMN seller_phone             text,
    ADD COLUMN seller_email             text,
    ADD COLUMN seller_payment_reference text,
    ADD COLUMN seller_payment_terms_days integer,
    ADD COLUMN seller_f_tax             boolean;

-- The immutability trigger already covers every column on these tables, so
-- the snapshot is frozen by the same rule as the rest of the document.
