-- rollback: safe
-- A function on Core's parties table and a replaced trigger on customers;
-- the previous release calls neither and makes customers as before.
--
-- A party that needs an invoice -- a car's buyer (#177), a renter (#199) --
-- gets its one customer, as invoices are addressed to customers (plan v2:
-- parties are linked 1:1 to customers, and customers do not grow into a
-- table for every party). The party's details move to the customer, as the
-- front desk keeps a customer's, and the party points at it
-- (parties_details_in_one_place). A party is never moved to another
-- customer, and gets one only once.
-- The application has no privilege to write a party's customer or person
-- (0069); this function makes the one move that is allowed, and only
-- that: a party of the current shop with no customer yet becomes the
-- customer of that shop it is given, its details cleared as they moved
-- there. Run with its owner's rights, so the application's privileges
-- stay as they are.
CREATE FUNCTION party_becomes_customer(party uuid, customer uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
BEGIN
    UPDATE parties p
       SET customer_id = customer, person_id = NULL, company_name = NULL, org_number = NULL, address_line1 = NULL,
           address_line2 = NULL, postal_code = NULL, city = NULL, country = NULL, revision = p.revision + 1
     WHERE p.id = party AND p.customer_id IS NULL
       AND p.shop_id = nullif(current_setting('app.current_shop', true), '')::uuid
       AND EXISTS (SELECT 1 FROM customers c WHERE c.id = customer AND c.shop_id = p.shop_id);
    IF NOT FOUND THEN
        RAISE EXCEPTION 'that party cannot become that customer' USING ERRCODE = 'check_violation';
    END IF;
END;
$$;
REVOKE ALL ON FUNCTION party_becomes_customer(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION party_becomes_customer(uuid, uuid) TO freesms_app;

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
