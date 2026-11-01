-- rollback: safe
-- A function replaced, no table or column. Every row the previous release
-- writes is one this accepts, but for the ones it exists to refuse: more
-- given back on an invoice, by any path, than the customer paid beyond it.
--
-- 0083 bounded an overpayment returned, and 0081 a refund by the payment it
-- refunds; neither bounded the two together, nor a receipt reversed after
-- money was given back on the strength of it (review round five). An
-- invoice of 1000 paid twice, 1000 returned as an overpayment and then the
-- second payment refunded as well, had 2000 given back for 1000 paid over.
-- The rule now is the invoice's, whatever the path: what has been given
-- back on it -- refunds and overpayments returned, less those reversed --
-- is at most what was paid beyond its total, credits counted. A shop that
-- has to give money back on an invoice that was paid exactly credits it
-- first, which is what makes the money owed back.
--
-- The invoice is locked first, as RecordPayment and RefundPayment lock it;
-- held is the invoice's outstanding after this row, given what has been
-- given back on it.
CREATE OR REPLACE FUNCTION payment_refunds_stay_within() RETURNS trigger AS $$
DECLARE
    orig invoice_payments;
    given bigint;
    held bigint;
BEGIN
    IF NEW.deposit_id IS NOT NULL THEN
        PERFORM pg_advisory_xact_lock(hashtextextended('deposit:' || NEW.deposit_id::text, 0));
    END IF;

    IF NEW.reverses_id IS NOT NULL THEN
        SELECT * INTO orig FROM invoice_payments WHERE id = NEW.reverses_id FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'a reversal of a payment that is not there' USING ERRCODE = 'foreign_key_violation';
        END IF;
        IF orig.reverses_id IS NOT NULL THEN
            RAISE EXCEPTION 'a reversal is not itself reversed' USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.amount_minor <> -orig.amount_minor OR NEW.rounding_minor <> -orig.rounding_minor
           OR NEW.invoice_id IS DISTINCT FROM orig.invoice_id OR NEW.deposit_id IS DISTINCT FROM orig.deposit_id
           OR NEW.payout_id IS DISTINCT FROM orig.payout_id THEN
            RAISE EXCEPTION 'a reversal undoes exactly the payment it points at' USING ERRCODE = 'check_violation';
        END IF;
        IF orig.refunds_id IS NOT NULL THEN
            -- Undoing a refund gives the payment back what it took: one
            -- step with any other refund of it.
            PERFORM 1 FROM invoice_payments WHERE id = orig.refunds_id FOR UPDATE;
        ELSIF EXISTS (SELECT 1 FROM invoice_payments r
                      WHERE r.refunds_id = orig.id
                        AND NOT EXISTS (SELECT 1 FROM invoice_payments x WHERE x.reverses_id = r.id)) THEN
            RAISE EXCEPTION 'a payment with refunds is reversed only once they are' USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    IF NEW.refunds_id IS NOT NULL THEN
        SELECT * INTO orig FROM invoice_payments WHERE id = NEW.refunds_id FOR UPDATE;
        IF orig.amount_minor <= 0 OR orig.reverses_id IS NOT NULL OR orig.refunds_id IS NOT NULL OR orig.method = 'deposit' THEN
            RAISE EXCEPTION 'only money received is refunded' USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.amount_minor >= 0
           OR NEW.invoice_id IS DISTINCT FROM orig.invoice_id OR NEW.deposit_id IS DISTINCT FROM orig.deposit_id
           OR NEW.payout_id IS DISTINCT FROM orig.payout_id THEN
            RAISE EXCEPTION 'a refund gives money back against what it was received for' USING ERRCODE = 'check_violation';
        END IF;
        IF EXISTS (SELECT 1 FROM invoice_payments x WHERE x.reverses_id = orig.id) THEN
            RAISE EXCEPTION 'a reversed payment is not refunded' USING ERRCODE = 'check_violation';
        END IF;
        -- What is refunded already, less any refund since reversed.
        SELECT coalesce(-sum(r.amount_minor), 0) - coalesce((SELECT sum(x.amount_minor) FROM invoice_payments x
                                                              WHERE x.reverses_id IN (SELECT id FROM invoice_payments WHERE refunds_id = orig.id)), 0)
          INTO given FROM invoice_payments r WHERE r.refunds_id = orig.id;
        IF given - NEW.amount_minor > orig.amount_minor THEN
            RAISE EXCEPTION 'a refund of more than was paid' USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    IF NEW.deposit_id IS NOT NULL AND NEW.amount_minor + NEW.rounding_minor < 0 THEN
        SELECT coalesce(sum(amount_minor + rounding_minor), 0) INTO held
          FROM invoice_payments WHERE deposit_id = NEW.deposit_id;
        IF held + NEW.amount_minor + NEW.rounding_minor < 0 THEN
            RAISE EXCEPTION 'more taken from a deposit than it holds' USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    -- Whatever takes money off an invoice -- a refund of one of its
    -- payments, an overpayment returned, a receipt reversed -- leaves
    -- what has been given back on it at most what the customer paid
    -- beyond what it owes. Given back is the refunds and the overpayments
    -- returned, less any of them reversed since; with anything given back,
    -- the invoice owes nothing after this row.
    IF NEW.invoice_id IS NOT NULL AND NEW.amount_minor + NEW.rounding_minor < 0 THEN
        PERFORM 1 FROM invoices WHERE id = NEW.invoice_id FOR UPDATE;
        SELECT i.gross_minor + coalesce((SELECT sum(c.gross_minor) FROM invoices c WHERE c.credit_of_id = i.id), 0)
               - coalesce((SELECT sum(p.amount_minor + p.rounding_minor) FROM invoice_payments p WHERE p.invoice_id = i.id), 0)
               - (NEW.amount_minor + NEW.rounding_minor)
          INTO held FROM invoices i WHERE i.id = NEW.invoice_id;
        WITH back AS (
            SELECT p.id, p.amount_minor + p.rounding_minor AS amount FROM invoice_payments p
             WHERE p.invoice_id = NEW.invoice_id
               AND (p.refunds_id IS NOT NULL
                    OR (p.reverses_id IS NULL AND p.method <> 'deposit' AND p.amount_minor + p.rounding_minor < 0)))
        SELECT -coalesce((SELECT sum(amount) FROM back), 0)
               - coalesce((SELECT sum(x.amount_minor + x.rounding_minor) FROM invoice_payments x WHERE x.reverses_id IN (SELECT id FROM back)), 0)
          INTO given;
        IF NEW.refunds_id IS NOT NULL OR (NEW.reverses_id IS NULL AND NEW.method <> 'deposit') THEN
            given := given - (NEW.amount_minor + NEW.rounding_minor);
        END IF;
        IF given > 0 AND held > 0 THEN
            RAISE EXCEPTION 'more given back on an invoice than is owed back' USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
