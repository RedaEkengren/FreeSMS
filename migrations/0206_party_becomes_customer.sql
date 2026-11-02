-- rollback: safe
-- Two column grants on Core's parties table and a trigger that holds them
-- to one change; the previous release updates neither column.
--
-- A party that needs an invoice -- a car's buyer (#177), a renter (#199) --
-- gets its one customer, as invoices are addressed to customers (plan v2:
-- parties are linked 1:1 to customers, and customers do not grow into a
-- table for every party). The party's details move to the customer, as the
-- front desk keeps a customer's, and the party points at it
-- (parties_details_in_one_place). A party is never moved to another
-- customer, and gets one only once.
GRANT UPDATE (customer_id, person_id) ON parties TO freesms_app;

CREATE FUNCTION parties_customer_once() RETURNS trigger AS $$
BEGIN
    IF OLD.customer_id IS DISTINCT FROM NEW.customer_id AND OLD.customer_id IS NOT NULL THEN
        RAISE EXCEPTION 'a party is never moved to another customer' USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.person_id IS DISTINCT FROM NEW.person_id AND NOT (OLD.customer_id IS NULL AND NEW.customer_id IS NOT NULL) THEN
        RAISE EXCEPTION 'a party''s person moves only to the customer it becomes' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER parties_customer_once BEFORE UPDATE OF customer_id, person_id ON parties
    FOR EACH ROW EXECUTE FUNCTION parties_customer_once();

-- A customer made for a party that exists (customerForPartyTx) is that
-- party's: the party making it says so for its transaction, and no second
-- party is made. Every other customer, made by any release, gets its party
-- as before.
CREATE OR REPLACE FUNCTION customer_has_a_party() RETURNS trigger AS $$
DECLARE
    party uuid;
BEGIN
    IF coalesce(current_setting('app.party_becoming_customer', true), '') <> '' THEN
        RETURN NEW;
    END IF;
    INSERT INTO parties (shop_id, kind, customer_id, created_at)
    VALUES (NEW.shop_id, NEW.kind, NEW.id, NEW.created_at)
    RETURNING id INTO party;
    INSERT INTO party_roles (shop_id, party_id, role, valid_from)
    VALUES (NEW.shop_id, party, 'customer', NEW.created_at);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
