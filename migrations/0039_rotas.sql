-- rollback: safe
-- New tables, and columns with defaults, that the previous release never
-- reads. The triggers it would run only notify.
--
-- Who is supposed to work when. The planner used to assume everybody was in
-- from 07:00 to 18:00 every weekday, and a person had nowhere to see their
-- own week.

-- Planning people is its own permission, given by whoever runs the shop: in
-- a larger workshop the workshop manager plans the staff and does not
-- change the bank account.
ALTER TABLE users ADD COLUMN plans_staff boolean NOT NULL DEFAULT false;

-- When the person last looked at their own schedule, so a change made by
-- somebody else since can wait on their list until they have.
ALTER TABLE users ADD COLUMN schedule_seen_at timestamptz;

-- A rota: a person's usual hours, repeating every `weeks` weeks from the
-- week of valid_from. A new rota is a new row from a date, never an edit,
-- so "what was Erik's rota in March" still has an answer.
CREATE TABLE rotas (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    valid_from date        NOT NULL,
    weeks      integer     NOT NULL CHECK (weeks BETWEEN 1 AND 4),
    created_by uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX rotas_user_idx ON rotas (user_id, valid_from);

-- A rota's hours: week 0 is the week valid_from falls in; weekday is ISO,
-- Monday 1. A shift that ends at or before it starts ends the next day.
CREATE TABLE rota_hours (
    id      uuid    PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id uuid    NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    rota_id uuid    NOT NULL REFERENCES rotas(id) ON DELETE RESTRICT,
    week    integer NOT NULL CHECK (week BETWEEN 0 AND 3),
    weekday integer NOT NULL CHECK (weekday BETWEEN 1 AND 7),
    starts  time    NOT NULL,
    ends    time    NOT NULL CHECK (ends <> starts),
    UNIQUE (rota_id, week, weekday)
);

-- One day's hours other than the rota's. The latest row for a person and a
-- day is the one that holds; both times empty is "back to the rota". Every
-- change stays, so a moved shift can be shown as moved.
CREATE TABLE shift_changes (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id    uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    user_id    uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    day        date        NOT NULL,
    starts     time,
    ends       time,
    created_by uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK ((starts IS NULL) = (ends IS NULL)),
    CHECK (starts IS NULL OR starts <> ends)
);
CREATE INDEX shift_changes_user_idx ON shift_changes (user_id, day, created_at);

-- When an absence was recorded, so a person can be told about one they did
-- not record themselves.
ALTER TABLE staff_absences ALTER COLUMN created_at SET DEFAULT clock_timestamp();

DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['rotas', 'rota_hours', 'shift_changes'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        -- Written once and kept: a rota is replaced by a newer one, a day by
        -- a later change, never edited.
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END
$$;

-- Open screens hear about it (0037): the booking planner, and the rota.
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON rotas
    FOR EACH ROW EXECUTE FUNCTION notify_change('calendar', 'staff');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON rota_hours
    FOR EACH ROW EXECUTE FUNCTION notify_change('calendar', 'staff');
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON shift_changes
    FOR EACH ROW EXECUTE FUNCTION notify_change('calendar', 'staff');
DROP TRIGGER notify_change ON staff_absences;
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON staff_absences
    FOR EACH ROW EXECUTE FUNCTION notify_change('calendar', 'staff');
