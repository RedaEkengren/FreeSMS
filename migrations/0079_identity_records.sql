-- rollback: safe
-- New tables, and one more kind of subject an audit event may be about. The
-- previous release reads and writes neither.
--
-- The schema has no national identity number and no driving licence on a
-- person, a customer or a party, on purpose (0002), and that stays so. When
-- Dealer or Rental has a real need -- money-laundering due diligence, an
-- ownership change, a licence checked before a car is handed over -- it is
-- kept here (#153): for one party, of one kind, for one purpose with its
-- legal basis, until it expires. The value is sealed by the application
-- with a key the database does not hold (internal/identity); there is no
-- plaintext column, so neither this database nor a dump of it can say what
-- a value was. Where a reference to a verification is enough -- the
-- default for a licence -- only the reference is kept.
CREATE TABLE identity_records (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id          uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    party_id         uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    kind             text        NOT NULL CHECK (kind IN ('personal_identity_number', 'licence_check', 'id_document_check')),
    -- The purposes are a list in the code, each with its legal basis and
    -- retention (workshop.IdentityPurposes); both are written here as they
    -- were when it was recorded.
    purpose          text        NOT NULL CHECK (purpose ~ '^[a-z_]{1,40}$'),
    legal_basis      text        NOT NULL CHECK (legal_basis IN ('legal_obligation', 'contract', 'legitimate_interest', 'consent')),
    recorded_by      uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    recorded_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at       timestamptz NOT NULL,
    -- The value, sealed: the key's id, and the nonce and ciphertext.
    key_id           text        CHECK (key_id IS NULL OR key_id ~ '^[a-zA-Z0-9_-]{1,40}$'),
    sealed           bytea,
    -- Or only that it was verified, and where.
    verification_ref text        CHECK (verification_ref IS NULL OR length(verification_ref) BETWEEN 1 AND 200),
    -- The purpose ended before its expiry: the rental cancelled.
    ended_at         timestamptz,
    ended_by         uuid        REFERENCES users(id) ON DELETE RESTRICT,
    end_reason       text        CHECK (end_reason IS NULL OR length(btrim(end_reason)) BETWEEN 1 AND 300),
    -- The value deleted -- at expiry, at the purpose's end, at a person's
    -- erasure -- and the record that a check was made kept.
    value_removed_at timestamptz,
    CHECK ((key_id IS NULL) = (sealed IS NULL)),
    CHECK (value_removed_at IS NOT NULL OR num_nonnulls(sealed, verification_ref) = 1),
    CHECK (value_removed_at IS NULL OR (sealed IS NULL AND verification_ref IS NULL)),
    CHECK (expires_at > recorded_at),
    CHECK ((ended_at IS NULL) = (ended_by IS NULL))
);
CREATE INDEX identity_records_party_idx ON identity_records (party_id);
CREATE INDEX identity_records_expiry_idx ON identity_records (expires_at) WHERE value_removed_at IS NULL;

-- How long a purpose keeps its values where the law leaves it to the shop
-- to say (G2): a setting, as rows, the latest counting.
CREATE TABLE identity_retention_settings (
    id      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    purpose text        NOT NULL CHECK (purpose ~ '^[a-z_]{1,40}$'),
    days    integer     NOT NULL CHECK (days BETWEEN 1 AND 3660),
    set_by  uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    set_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX identity_retention_settings_idx ON identity_retention_settings (shop_id, purpose, set_at DESC);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['identity_records', 'identity_retention_settings'] LOOP
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
GRANT UPDATE (key_id, sealed, verification_ref, ended_at, ended_by, end_reason, value_removed_at) ON identity_records TO freesms_app;

-- What a record says it is stays: only its value may be sealed again with
-- a newer key, its purpose ended, and its value removed -- once removed,
-- never put back.
CREATE FUNCTION identity_records_keep_their_facts() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'an identity record is never deleted; its value is removed' USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.value_removed_at IS NOT NULL AND (NEW.sealed IS NOT NULL OR NEW.verification_ref IS NOT NULL
                                             OR NEW.value_removed_at IS DISTINCT FROM OLD.value_removed_at) THEN
        RAISE EXCEPTION 'a removed identity value is not put back' USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.ended_at IS NOT NULL AND NEW.ended_at IS DISTINCT FROM OLD.ended_at THEN
        RAISE EXCEPTION 'an ended purpose stays ended' USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.verification_ref IS NOT NULL AND NEW.verification_ref IS DISTINCT FROM OLD.verification_ref THEN
        RAISE EXCEPTION 'a verification reference is not changed' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER identity_records_keep_their_facts BEFORE UPDATE OR DELETE ON identity_records
    FOR EACH ROW EXECUTE FUNCTION identity_records_keep_their_facts();

-- Every read of a value is an audit event about the record.
ALTER TABLE audit_events DROP CONSTRAINT audit_events_subject_kind_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_subject_kind_check
    CHECK (subject_kind IN ('shop', 'user', 'resource_block', 'document', 'invoice', 'payment', 'party', 'customer',
                            'vehicle', 'work_order', 'identity_record'));
