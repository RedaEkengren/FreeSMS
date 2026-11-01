-- rollback: safe
-- Two new tables, filled from customers and kept filled by a trigger on
-- customers, so a customer the previous release creates gets its party as
-- well; one more subject the change log accepts, which the previous release
-- never writes. The previous release reads none of it.
--
-- A party is one real-world person or company the business has dealings
-- with (#148): the workshop's customer, the private seller of a car, its
-- buyer, a consignor, a supplier, a renter -- one record, with the history
-- of those relationships as rows. Car sales and rental reference parties;
-- the workshop keeps working through customers, and references move over
-- step by step. customers is not widened into a table for every party.
--
-- Every customer has exactly one party, and a party at most one customer:
-- customer_id is unique, every customer there is gets its party here, and
-- every customer made from now on gets one in the same transaction, by the
-- trigger below. A customer's party has no details of its own -- they stay
-- on the customer and its person, where the workshop edits them, so there is
-- one place each fact is kept. A party that is no customer -- the private
-- seller of a car -- has its own: a person for a private party, as a
-- customer has, and a company's name and number for a company.
--
-- No national identity number, here or anywhere on a party: that is an
-- identity record, kept apart with a purpose and an expiry (#153).
CREATE TABLE parties (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id       uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    kind          text        NOT NULL CHECK (kind IN ('private', 'company')),
    customer_id   uuid        UNIQUE REFERENCES customers(id) ON DELETE RESTRICT,
    person_id     uuid        REFERENCES people(id) ON DELETE RESTRICT,
    company_name  text        CHECK (company_name IS NULL OR length(company_name) BETWEEN 1 AND 200),
    org_number    text        CHECK (org_number IS NULL OR length(org_number) <= 40),
    address_line1 text        CHECK (address_line1 IS NULL OR length(address_line1) <= 200),
    address_line2 text        CHECK (address_line2 IS NULL OR length(address_line2) <= 200),
    postal_code   text        CHECK (postal_code IS NULL OR length(postal_code) <= 20),
    city          text        CHECK (city IS NULL OR length(city) <= 100),
    country       char(2),
    revision      integer     NOT NULL DEFAULT 1,
    -- The same person entered twice, in two modules, before anyone noticed:
    -- merged as customers are (0049), the other kept as a marked record.
    merged_into   uuid        REFERENCES parties(id) ON DELETE RESTRICT,
    merged_at     timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CONSTRAINT parties_merged_whole CHECK ((merged_into IS NULL) = (merged_at IS NULL)),
    CONSTRAINT parties_not_merged_into_itself CHECK (merged_into IS DISTINCT FROM id),
    CONSTRAINT parties_details_in_one_place CHECK (
        (customer_id IS NOT NULL AND person_id IS NULL AND company_name IS NULL AND org_number IS NULL
         AND address_line1 IS NULL AND address_line2 IS NULL AND postal_code IS NULL AND city IS NULL AND country IS NULL)
        OR (customer_id IS NULL AND kind = 'private' AND person_id IS NOT NULL AND company_name IS NULL)
        OR (customer_id IS NULL AND kind = 'company' AND company_name IS NOT NULL)
    )
);
CREATE INDEX parties_shop_idx ON parties (shop_id);
CREATE INDEX parties_person_idx ON parties (person_id) WHERE person_id IS NOT NULL;

ALTER TABLE parties ENABLE ROW LEVEL SECURITY;
ALTER TABLE parties FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON parties
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- Never deleted, and never moved to another customer: a party is what
-- documents and roles point at.
GRANT SELECT, INSERT ON parties TO freesms_app;
GRANT UPDATE (company_name, org_number, address_line1, address_line2, postal_code, city, country,
              revision, merged_into, merged_at) ON parties TO freesms_app;

-- What a party is to the business, and for how long. A role is a row with a
-- period, never a boolean: a private seller who later rents is a seller from
-- one day and a renter from another, and "since when" is the question.
CREATE TABLE party_roles (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    party_id   uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    role       text        NOT NULL CHECK (role IN ('customer', 'seller', 'buyer', 'supplier', 'consignor', 'renter')),
    valid_from timestamptz NOT NULL DEFAULT clock_timestamp(),
    valid_to   timestamptz,
    started_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    ended_by   uuid        REFERENCES users(id) ON DELETE RESTRICT,
    note       text        CHECK (note IS NULL OR length(note) <= 300),
    CHECK (valid_to IS NULL OR valid_to > valid_from),
    CHECK (ended_by IS NULL OR valid_to IS NOT NULL)
);
CREATE INDEX party_roles_party_idx ON party_roles (party_id, valid_from);
-- One open period of each role per party.
CREATE UNIQUE INDEX party_roles_one_open ON party_roles (party_id, role) WHERE valid_to IS NULL;

ALTER TABLE party_roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE party_roles FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON party_roles
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);

-- A period is ended, never rewritten.
GRANT SELECT, INSERT ON party_roles TO freesms_app;
GRANT UPDATE (valid_to, ended_by) ON party_roles TO freesms_app;

-- Every customer there is, and its role as one since it was entered.
INSERT INTO parties (shop_id, kind, customer_id, created_at)
SELECT shop_id, kind, id, created_at FROM customers;
INSERT INTO party_roles (shop_id, party_id, role, valid_from)
SELECT p.shop_id, p.id, 'customer', p.created_at FROM parties p;
-- Merged customers' parties are merged the same way.
UPDATE parties d SET merged_into = k.id, merged_at = c.merged_at
FROM customers c JOIN parties k ON k.customer_id = c.merged_into
WHERE d.customer_id = c.id AND c.merged_into IS NOT NULL;

-- Every customer made from now on, by any release, gets its party in the
-- same transaction.
CREATE FUNCTION customer_has_a_party() RETURNS trigger AS $$
DECLARE
    party uuid;
BEGIN
    INSERT INTO parties (shop_id, kind, customer_id, created_at)
    VALUES (NEW.shop_id, NEW.kind, NEW.id, NEW.created_at)
    RETURNING id INTO party;
    INSERT INTO party_roles (shop_id, party_id, role, valid_from)
    VALUES (NEW.shop_id, party, 'customer', NEW.created_at);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER customer_has_a_party AFTER INSERT ON customers
    FOR EACH ROW EXECUTE FUNCTION customer_has_a_party();

-- What a merged-away party is now, as customer_now (0068).
CREATE FUNCTION party_now(p uuid) RETURNS uuid LANGUAGE sql STABLE AS $$
    WITH RECURSIVE up(id, next, depth) AS (
        SELECT id, merged_into, 0 FROM parties WHERE id = p
        UNION ALL
        SELECT k.id, k.merged_into, up.depth + 1 FROM parties k JOIN up ON k.id = up.next WHERE up.depth < 64
    )
    SELECT id FROM up ORDER BY depth DESC LIMIT 1
$$;

-- A change to a party is logged as one to a customer is.
ALTER TABLE record_changes DROP CONSTRAINT record_changes_subject_check;
ALTER TABLE record_changes ADD CONSTRAINT record_changes_subject_check
    CHECK (subject IN ('customer', 'vehicle', 'ownership', 'registration', 'merge', 'party'));
