-- rollback: safe
-- A function replaced, no table or column. Every row the previous release
-- writes is one this accepts, but for the ones it exists to refuse: more
-- handed back for a payment than it was, and more taken from a deposit than
-- it holds. The previous release refuses those itself only in part, and is
-- refused here for the rest, which is the point.
--
-- Money handed back for a payment -- its refunds, less any reversed since,
-- and its own reversal -- never adds up to more than it was (#152, review
-- round four). 0078 counted the refunds only, so a payment reversed and
-- then refunded was handed back twice. Three rules, held here and not only
-- in Go:
--
--   * A reversed payment is not refunded: it never arrived.
--   * A payment with refunds standing is not reversed. A reversal says the
--     payment was recorded in error and undoes exactly it, the whole amount
--     (freesms -verify checks that); a refund that did happen is money that
--     moved, and is reversed first if it was the mistake. Reversing only
--     what is left would make a reversal that is not the payment's negative,
--     and a ledger where "reversed" no longer means "never happened".
--   * A reversal undoes exactly one payment that is not itself a reversal:
--     the same thing, the same money, the opposite sign.
--
-- And a deposit never holds less than nothing: whatever is taken from it --
-- applied, refunded, or its receipt reversed -- is taken from what it still
-- holds.
--
-- Each check locks what it counts first: the deposit (the same advisory
-- lock the application takes, so the two are one queue), then the payment
-- handed back for. A statement in a function takes a fresh snapshot under
-- read committed, so what is counted after the lock includes whatever the
-- holder of the lock committed.
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
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
