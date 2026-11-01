-- rollback: safe
-- New columns with defaults, a CHECK every existing row meets, a NOT NULL
-- relaxed under that CHECK, and a trigger on insert that every credit note
-- the previous release writes passes. The previous release reads
-- invoices.work_order_id as always set, and it is for every row this
-- release can write: the workshop is the only thing that issues invoices in
-- this release. A manual invoice is an operation without a page until the
-- next release (workshop.IssueManual), and a vehicle sale or a rental has no
-- module to issue it yet (workshop.Available). So no row the previous
-- release cannot read is written before the release that writes them is the
-- one running -- the split across releases DEPLOY.md asks for.
--
-- An invoice has a source (#146): the workshop's job, a vehicle sale, a
-- rental, or a sale entered by hand. The gap-free series, the frozen lines,
-- the immutability and the credit notes stay one machine for all of them.
--
-- Every invoice already issued is the workshop's, and reads so through the
-- column's default. Adding a column with a constant default writes nothing to
-- the existing rows -- Postgres keeps the default in the catalogue and reads
-- it for rows written before it -- so no issued invoice changes by a byte and
-- the immutability trigger is never asked. An UPDATE to backfill it would be
-- refused by that trigger, and should be.
ALTER TABLE invoices
    ADD COLUMN source text NOT NULL DEFAULT 'workshop'
        CHECK (source IN ('workshop', 'vehicle_sale', 'rental', 'manual')),
    -- One reference per source. The sale and the rental tables come with
    -- their modules (#172, #189), and their migrations add the foreign
    -- keys; until then nothing writes these.
    ADD COLUMN vehicle_sale_id uuid,
    ADD COLUMN rental_id uuid,
    -- Whom an invoice that is not the workshop's is to. The workshop's reach
    -- their customer through the job, as they always have; the others have
    -- no job, and a person's copy of their data, a merge and an erasure must
    -- still find them.
    ADD COLUMN customer_id uuid REFERENCES customers(id) ON DELETE RESTRICT;

-- Exactly the reference that matches the source, and a customer for every
-- invoice without a job. Before the NOT NULL goes, so that there is no
-- moment at which an invoice can have no job and no rule.
ALTER TABLE invoices ADD CONSTRAINT invoices_source_reference CHECK (
    (source = 'workshop') = (work_order_id IS NOT NULL)
    AND (source = 'vehicle_sale') = (vehicle_sale_id IS NOT NULL)
    AND (source = 'rental') = (rental_id IS NOT NULL)
    AND (source = 'workshop' OR customer_id IS NOT NULL)
);
ALTER TABLE invoices ALTER COLUMN work_order_id DROP NOT NULL;

CREATE INDEX invoices_customer_idx ON invoices (customer_id) WHERE customer_id IS NOT NULL;

-- A credit note is of the same thing as the invoice it credits: the same
-- source, the same job, sale or rental, the same customer. Read under row
-- level security, so the original is the caller's shop's or nothing.
CREATE FUNCTION invoices_credit_keeps_source() RETURNS trigger AS $$
DECLARE
    o invoices%ROWTYPE;
BEGIN
    IF NEW.credit_of_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT * INTO o FROM invoices WHERE id = NEW.credit_of_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'a credit note must credit an invoice of this shop'
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF NEW.source IS DISTINCT FROM o.source
       OR NEW.work_order_id IS DISTINCT FROM o.work_order_id
       OR NEW.vehicle_sale_id IS DISTINCT FROM o.vehicle_sale_id
       OR NEW.rental_id IS DISTINCT FROM o.rental_id
       OR NEW.customer_id IS DISTINCT FROM o.customer_id THEN
        RAISE EXCEPTION 'a credit note keeps the source of the invoice it credits (% %, not % %)',
            o.source, o.id, NEW.source, coalesce(NEW.work_order_id, NEW.vehicle_sale_id, NEW.rental_id)
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER invoices_credit_keeps_source
    BEFORE INSERT ON invoices
    FOR EACH ROW EXECUTE FUNCTION invoices_credit_keeps_source();
