-- rollback: safe
-- A new table; damage_cases is left as the previous release reads it, its
-- state staying 'observed', which that release shows.
--
-- What happens to a damage case (#197) is rows, each a person's step with
-- its reason: whether the damage is new or was there before, who is
-- liable and on what basis, what it comes to and on what basis, how the
-- customer was told, how they answered, and the outcome. A damage is never
-- a debt by itself (PMÖD 2019:11): it becomes money owed only through an
-- outcome, and that outcome is refused before the amount is settled and
-- the customer told. Reopened, a case gets a step, never an edit.
CREATE TABLE damage_case_steps (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    case_id     uuid        NOT NULL REFERENCES damage_cases(id) ON DELETE RESTRICT,
    seq         bigint      GENERATED ALWAYS AS IDENTITY,
    step        text        NOT NULL CHECK (step IN ('new_confirmed', 'pre_existing', 'liability_assessed', 'amount_settled',
                                                     'customer_informed', 'customer_response', 'outcome', 'reopened')),
    liability   text        CHECK (liability IS NULL OR liability IN ('renter', 'shop', 'third_party', 'unknown')),
    amount_minor bigint     CHECK (amount_minor IS NULL OR amount_minor BETWEEN 0 AND 10000000000),
    -- How the customer was told, how they answered, and how it ended.
    channel     text        CHECK (channel IS NULL OR channel IN ('email', 'sms', 'letter', 'in_person', 'phone')),
    response    text        CHECK (response IS NULL OR response IN ('accepted', 'disputed', 'no_answer')),
    outcome     text        CHECK (outcome IS NULL OR outcome IN ('invoiced', 'set_against_deposit', 'written_off',
                                                                 'insurance_claim', 'closed_not_renter')),
    reason      text        NOT NULL CHECK (length(btrim(reason)) BETWEEN 1 AND 500),
    at          timestamptz NOT NULL DEFAULT clock_timestamp(),
    by_user     uuid        REFERENCES users(id) ON DELETE RESTRICT,
    CHECK ((step = 'liability_assessed') = (liability IS NOT NULL)),
    CHECK ((step = 'amount_settled') = (amount_minor IS NOT NULL)),
    CHECK ((step = 'customer_informed') = (channel IS NOT NULL)),
    CHECK ((step = 'customer_response') = (response IS NOT NULL)),
    CHECK ((step = 'outcome') = (outcome IS NOT NULL))
);
CREATE INDEX damage_case_steps_idx ON damage_case_steps (case_id, seq);

ALTER TABLE damage_case_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE damage_case_steps FORCE ROW LEVEL SECURITY;
CREATE POLICY shop_isolation ON damage_case_steps
    USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
    WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid);
REVOKE ALL ON damage_case_steps FROM freesms_app;
GRANT SELECT, INSERT ON damage_case_steps TO freesms_app;
CREATE TRIGGER notify_change AFTER INSERT OR UPDATE OR DELETE ON damage_case_steps
    FOR EACH ROW EXECUTE FUNCTION notify_change('actions');

SELECT widen_text_check('audit_events', 'audit_events_subject_kind_check', 'subject_kind', ARRAY['damage_case']);
