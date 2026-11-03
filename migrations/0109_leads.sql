-- rollback: safe
-- New tables and a view. Nothing in the previous release reads or writes
-- them.
--
-- A lead (#172): someone asking about a car -- from an ad form, the
-- dealer's website, a phone call, a walk-in or an email -- with the car
-- or what they want, the salesperson who has it, and the next step. One
-- person asking about three cars is three leads.
--
-- What stands about a lead -- where it is, who has it, what is next -- is
-- the latest of its events, never a column written over: a lead passed
-- from one salesperson to another, or lost and taken up again, keeps
-- what was before. Each contact with the person is a row of its own.
CREATE TABLE leads (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id              uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    -- The person, as a party once one is needed (#148); until then only a
    -- name and a way to reach them.
    party_id             uuid        REFERENCES parties(id) ON DELETE RESTRICT,
    contact_name         text        CHECK (contact_name IS NULL OR length(btrim(contact_name)) BETWEEN 1 AND 200),
    contact_phone        text        CHECK (contact_phone IS NULL OR length(contact_phone) <= 40),
    -- The phone's digits alone, and the email in lower case: what a second
    -- enquiry from the same person is recognised by.
    contact_phone_digits text        CHECK (contact_phone_digits IS NULL OR contact_phone_digits ~ '^[0-9]{5,20}$'),
    contact_email        text        CHECK (contact_email IS NULL OR (length(contact_email) <= 200 AND contact_email = lower(contact_email))),
    -- The car asked about, or none: any car, with what they want.
    stock_unit_id        uuid        REFERENCES stock_units(id) ON DELETE RESTRICT,
    wishes               text        CHECK (wishes IS NULL OR length(wishes) <= 1000),
    channel              text        NOT NULL CHECK (channel IN ('ad_form', 'website', 'phone', 'walk_in', 'email', 'other')),
    -- A form or a phone sending the same enquiry twice finds the lead it
    -- made.
    request_key          text        CHECK (request_key IS NULL OR length(request_key) <= 100),
    created_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by           uuid        REFERENCES users(id) ON DELETE RESTRICT,
    -- When the person's details were erased from a lead that never became
    -- a deal; what is left still counts in the statistics.
    erased_at            timestamptz,
    -- Someone to reach, until erased.
    CHECK (erased_at IS NOT NULL OR party_id IS NOT NULL
           OR (contact_name IS NOT NULL AND (contact_phone_digits IS NOT NULL OR contact_email IS NOT NULL))),
    -- A car, or what they want, until erased.
    CHECK (erased_at IS NOT NULL OR stock_unit_id IS NOT NULL OR wishes IS NOT NULL)
);
CREATE UNIQUE INDEX leads_request_idx ON leads (shop_id, request_key) WHERE request_key IS NOT NULL;
CREATE INDEX leads_phone_idx ON leads (shop_id, contact_phone_digits) WHERE contact_phone_digits IS NOT NULL;
CREATE INDEX leads_email_idx ON leads (shop_id, contact_email) WHERE contact_email IS NOT NULL;
CREATE INDEX leads_unit_idx ON leads (stock_unit_id) WHERE stock_unit_id IS NOT NULL;

-- What happened to a lead, in order: where it stands now (status), who
-- has it (assigned, or nobody), and its next step (when and what, or none).
CREATE TABLE lead_events (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id        uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    lead_id        uuid        NOT NULL REFERENCES leads(id) ON DELETE RESTRICT,
    seq            bigint      GENERATED ALWAYS AS IDENTITY,
    kind           text        NOT NULL CHECK (kind IN ('status', 'assigned', 'next_step')),
    status         text        CHECK (status IN ('new', 'contacted', 'test_drive_booked', 'offer_made', 'won', 'lost')),
    salesperson_id uuid        REFERENCES users(id) ON DELETE RESTRICT,
    next_step_on   date,
    -- The step, or why the lead was lost, in the salesperson's words.
    note           text        CHECK (note IS NULL OR length(note) <= 500),
    lost_reason    text        CHECK (lost_reason IN ('bought_elsewhere', 'price', 'financing', 'car_sold', 'trade_in_value',
                                                       'no_response', 'changed_mind', 'other')),
    at             timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user        uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((kind = 'status') = (status IS NOT NULL)),
    CHECK ((status = 'lost') = (lost_reason IS NOT NULL) OR status IS NULL),
    CHECK (kind = 'status' OR lost_reason IS NULL),
    CHECK (kind = 'assigned' OR salesperson_id IS NULL),
    CHECK (kind = 'next_step' OR next_step_on IS NULL)
);
CREATE INDEX lead_events_lead_idx ON lead_events (lead_id, kind, seq DESC);

-- Each time somebody reached the person, or the person them.
CREATE TABLE lead_contacts (
    id      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    lead_id uuid        NOT NULL REFERENCES leads(id) ON DELETE RESTRICT,
    at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user uuid        REFERENCES users(id) ON DELETE RESTRICT,
    how     text        NOT NULL CHECK (how IN ('phone', 'sms', 'email', 'in_person', 'other')),
    note    text        CHECK (note IS NULL OR length(note) <= 1000)
);
CREATE INDEX lead_contacts_lead_idx ON lead_contacts (lead_id, at);

-- How long a lead that never became a deal keeps the person's details.
-- A setting changed is a row.
CREATE TABLE lead_settings (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id            uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    seq                bigint      GENERATED ALWAYS AS IDENTITY,
    erase_after_months integer     NOT NULL CHECK (erase_after_months BETWEEN 1 AND 120),
    changed_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    changed_by         uuid        REFERENCES users(id) ON DELETE RESTRICT
);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['leads', 'lead_events', 'lead_contacts', 'lead_settings'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        -- The default privileges (0018) give everything; revoke first (0071).
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END;
$$;
-- The one change after the fact: erasing the person from a lead that
-- never became a deal, and what was written about them.
GRANT UPDATE (party_id, contact_name, contact_phone, contact_phone_digits, contact_email, wishes, erased_at)
    ON leads TO freesms_app;
GRANT UPDATE (note) ON lead_contacts TO freesms_app;
GRANT UPDATE (note) ON lead_events TO freesms_app;

-- Where each lead stands now: the latest of each kind of event.
CREATE VIEW lead_now WITH (security_invoker = true) AS
SELECT l.id AS lead_id,
       s.status, s.lost_reason, s.note AS lost_note,
       a.salesperson_id,
       n.next_step_on, n.note AS next_step,
       greatest(l.created_at, (SELECT max(e.at) FROM lead_events e WHERE e.lead_id = l.id),
                (SELECT max(c.at) FROM lead_contacts c WHERE c.lead_id = l.id)) AS last_activity_at
  FROM leads l
  LEFT JOIN LATERAL (SELECT e.status, e.lost_reason, e.note FROM lead_events e
                      WHERE e.lead_id = l.id AND e.kind = 'status' ORDER BY e.seq DESC LIMIT 1) s ON true
  LEFT JOIN LATERAL (SELECT e.salesperson_id FROM lead_events e
                      WHERE e.lead_id = l.id AND e.kind = 'assigned' ORDER BY e.seq DESC LIMIT 1) a ON true
  LEFT JOIN LATERAL (SELECT e.next_step_on, e.note FROM lead_events e
                      WHERE e.lead_id = l.id AND e.kind = 'next_step' ORDER BY e.seq DESC LIMIT 1) n ON true;
GRANT SELECT ON lead_now TO freesms_app;
