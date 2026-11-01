-- rollback: safe
-- A new table the previous release neither reads nor writes, and a check
-- that fires only when a deposit's application is reversed. The previous
-- release reverses either half alone, which is what is refused here; every
-- other row it writes is accepted. An application it writes has no link
-- row, so it cannot be reversed until this release is back, which is the
-- safe way round.
--
-- A deposit applied to an invoice is two rows (0078): out of the deposit,
-- into the invoice, with no money moving. They are one fact. Reversing one
-- alone made the same money exist twice -- held by the deposit again and
-- paid on the invoice -- or vanish. This names the pair, and a reversal of
-- either half is refused at commit unless the other half is reversed in the
-- same transaction.
CREATE TABLE deposit_applications (
    shop_id            uuid NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    deposit_payment_id uuid NOT NULL UNIQUE REFERENCES invoice_payments(id) ON DELETE RESTRICT,
    invoice_payment_id uuid NOT NULL UNIQUE REFERENCES invoice_payments(id) ON DELETE RESTRICT,
    PRIMARY KEY (deposit_payment_id, invoice_payment_id)
);
ALTER TABLE deposit_applications ENABLE ROW LEVEL SECURITY;
ALTER TABLE deposit_applications FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON deposit_applications
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
-- The default privileges (0018) give everything; revoke first (0071).
REVOKE ALL ON deposit_applications FROM freesms_app;
GRANT SELECT, INSERT ON deposit_applications TO freesms_app;

-- The two halves of each application already written, found by the
-- reference the invoice's half carries: the deposit half's number.
INSERT INTO deposit_applications (shop_id, deposit_payment_id, invoice_payment_id)
SELECT d.shop_id, d.id, i.id
  FROM invoice_payments d
  JOIN invoice_payments i ON i.shop_id = d.shop_id AND i.method = 'deposit' AND i.invoice_id IS NOT NULL
                         AND i.reverses_id IS NULL AND i.reference = d.series || '-' || d.number
 WHERE d.method = 'deposit' AND d.deposit_id IS NOT NULL AND d.reverses_id IS NULL
ON CONFLICT DO NOTHING;

-- The two halves are written together, each the other's negative, one out
-- of a deposit and one into an invoice.
CREATE FUNCTION deposit_application_is_a_pair() RETURNS trigger AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM invoice_payments d JOIN invoice_payments i ON i.id = NEW.invoice_payment_id
                   WHERE d.id = NEW.deposit_payment_id AND d.method = 'deposit' AND i.method = 'deposit'
                     AND d.deposit_id IS NOT NULL AND i.invoice_id IS NOT NULL
                     AND d.reverses_id IS NULL AND i.reverses_id IS NULL
                     AND d.amount_minor = -i.amount_minor AND d.amount_minor < 0) THEN
        RAISE EXCEPTION 'a deposit application is a payment out of a deposit and the same into an invoice'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER deposit_application_is_a_pair BEFORE INSERT ON deposit_applications
    FOR EACH ROW EXECUTE FUNCTION deposit_application_is_a_pair();

-- Checked at commit, so the two reversals can be written one after the
-- other: either half reversed means both are.
CREATE FUNCTION deposit_application_reversed_whole() RETURNS trigger AS $$
DECLARE
    other uuid;
BEGIN
    IF NEW.reverses_id IS NULL OR NEW.method <> 'deposit' THEN
        RETURN NULL;
    END IF;
    SELECT CASE WHEN a.deposit_payment_id = NEW.reverses_id THEN a.invoice_payment_id ELSE a.deposit_payment_id END
      INTO other FROM deposit_applications a
     WHERE NEW.reverses_id IN (a.deposit_payment_id, a.invoice_payment_id);
    IF other IS NULL THEN
        RAISE EXCEPTION 'a deposit application is reversed with its other half, which cannot be found'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM invoice_payments x WHERE x.reverses_id = other) THEN
        RAISE EXCEPTION 'a deposit application is reversed whole, both halves together'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
CREATE CONSTRAINT TRIGGER deposit_application_reversed_whole AFTER INSERT ON invoice_payments
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION deposit_application_reversed_whole();
