-- rollback: safe
-- New tables, a nullable column and a widened CHECK. The previous release never
-- reads the tables or the column and never writes a message about a campaign.
--
-- A seasonal message to every customer with a set in storage (#230). A
-- campaign has a season and a text; the people it reaches are worked out when it
-- runs, from the sets in a place that day. A customer is reached once per
-- campaign, however many sets or cars they have: the recipient row is the
-- proof, and a run repeated finds it there and queues nothing more.
--
-- Whether a customer wants shop messages is the newest of their choices, so one
-- who opted out and came back leaves both. It stops campaigns only: a message
-- about a job the customer has with the shop is not marketing.
CREATE TABLE customer_message_choices (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    customer_id uuid        NOT NULL REFERENCES customers(id) ON DELETE RESTRICT,
    seq         bigint      GENERATED ALWAYS AS IDENTITY,
    kind        text        NOT NULL CHECK (kind IN ('opted_out', 'opted_in')),
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user     uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX customer_message_choices_idx ON customer_message_choices (customer_id, seq);

CREATE TABLE tyre_campaigns (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    name       text        NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
    -- The kind of set it is about: summer, winter (studded or friction) or
    -- all-season.
    season     text        NOT NULL CHECK (season IN ('summer', 'winter', 'all_season')),
    subject    text        NOT NULL DEFAULT '' CHECK (length(subject) <= 200),
    -- {sets} is replaced with the customer's own sets.
    body       text        NOT NULL CHECK (length(btrim(body)) BETWEEN 1 AND 1200),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    created_by uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (shop_id, name)
);

CREATE TABLE tyre_campaign_recipients (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    campaign_id uuid        NOT NULL REFERENCES tyre_campaigns(id) ON DELETE RESTRICT,
    customer_id uuid        NOT NULL REFERENCES customers(id) ON DELETE RESTRICT,
    message_id  uuid        NOT NULL REFERENCES outbound_messages(id) ON DELETE RESTRICT,
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (campaign_id, customer_id)
);

ALTER TABLE outbound_messages ADD COLUMN campaign_id uuid REFERENCES tyre_campaigns(id) ON DELETE RESTRICT;
CREATE INDEX outbound_messages_campaign_idx ON outbound_messages (campaign_id) WHERE campaign_id IS NOT NULL;
ALTER TABLE outbound_messages DROP CONSTRAINT IF EXISTS outbound_messages_about_something;
ALTER TABLE outbound_messages ADD CONSTRAINT outbound_messages_about_something
    CHECK (work_order_id IS NOT NULL OR booking_id IS NOT NULL OR campaign_id IS NOT NULL);
ALTER TABLE outbound_messages DROP CONSTRAINT IF EXISTS outbound_messages_about_check;
ALTER TABLE outbound_messages ADD CONSTRAINT outbound_messages_about_check
    CHECK (about IN ('ready', 'answer', 'waiting_part', 'invoice', 'reminder',
                     'booking_confirm', 'booking_reminder', 'booking_moved', 'booking_cancelled', 'campaign'));

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['customer_message_choices', 'tyre_campaigns', 'tyre_campaign_recipients'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END;
$$;
