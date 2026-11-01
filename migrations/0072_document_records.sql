-- rollback: safe
-- New tables only; the previous release reads and writes none of them.
--
-- A contract is a document whose content is frozen when it is produced, and
-- whose signing is a separate fact recorded after (#150): a purchase
-- agreement, a sales contract, a test-drive agreement, a consignment
-- agreement, a rental agreement. Paper signing, recorded, works from the
-- first day; an e-signature service is a Signer plugged in later (#212)
-- without changing any of this.

-- The shop's own wording, in versions. A version is never changed: editing
-- a template writes the next version, so a document says exactly which
-- words it was made from, and a version already used cannot be made to say
-- something else. Not translated: a Swedish shop writes its contracts in
-- Swedish, and they go to its customers as written.
CREATE TABLE document_templates (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    kind        text        NOT NULL CHECK (kind IN ('purchase_agreement', 'sales_contract', 'test_drive_agreement',
                                                     'consignment_agreement', 'rental_agreement')),
    name        text        NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid        REFERENCES users(id) ON DELETE RESTRICT
);
CREATE INDEX document_templates_shop_idx ON document_templates (shop_id);

CREATE TABLE document_template_versions (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    template_id uuid        NOT NULL REFERENCES document_templates(id) ON DELETE RESTRICT,
    version     integer     NOT NULL CHECK (version > 0),
    body        text        NOT NULL CHECK (length(body) BETWEEN 1 AND 20000),
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid        REFERENCES users(id) ON DELETE RESTRICT,
    UNIQUE (template_id, version)
);

-- One document as it was produced. body is what it says, rendered from the
-- template version with data; data is what was put in, kept so the text can
-- be shown to have come from it; content_sha256 is the hash of all of it and
-- of the parties as named, which a signature names to say what was signed.
-- A correction is a new document superseding this one, which stays.
CREATE TABLE document_records (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id             uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    kind                text        NOT NULL CHECK (kind IN ('purchase_agreement', 'sales_contract', 'test_drive_agreement',
                                                             'consignment_agreement', 'rental_agreement')),
    -- What it is about. Their tables come with their modules (#160, #172,
    -- #189), whose migrations add the foreign keys.
    subject_kind        text        NOT NULL CHECK (subject_kind IN ('sale', 'purchase', 'rental_booking', 'consignment', 'test_drive')),
    subject_id          uuid        NOT NULL,
    template_version_id uuid        NOT NULL REFERENCES document_template_versions(id) ON DELETE RESTRICT,
    title               text        NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    body                text,
    data                jsonb,
    content_sha256      bytea       NOT NULL CHECK (length(content_sha256) = 32),
    supersedes_id       uuid        UNIQUE REFERENCES document_records(id) ON DELETE RESTRICT,
    created_at          timestamptz NOT NULL DEFAULT now(),
    created_by          uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    -- Blanked past its retention at a person's request: what it said goes,
    -- that it existed, its hash and its signatures stay.
    erased_at           timestamptz,
    CHECK ((erased_at IS NULL) = (body IS NOT NULL AND data IS NOT NULL))
);
CREATE INDEX document_records_subject_idx ON document_records (subject_id);

-- The parties as the document names them, copied when it is produced: a
-- party's later change of address does not change a contract.
CREATE TABLE document_record_parties (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id     uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    document_id uuid        NOT NULL REFERENCES document_records(id) ON DELETE RESTRICT,
    party_id    uuid        NOT NULL REFERENCES parties(id) ON DELETE RESTRICT,
    position    integer     NOT NULL,
    role        text        NOT NULL CHECK (role IN ('buyer', 'seller', 'renter', 'consignor', 'driver', 'guarantor')),
    name        text        NOT NULL,
    org_number  text,
    address     text,
    UNIQUE (document_id, position),
    UNIQUE (document_id, party_id, role)
);
CREATE INDEX document_record_parties_party_idx ON document_record_parties (party_id);

-- A signature is evidence, as a row: which party, how, when, recorded by
-- whom, the provider's reference or the scan, who witnessed it on paper,
-- and the hash of the document signed. One per party on the document; a
-- document is unsigned, partly or fully signed by what is here.
CREATE TABLE document_signatures (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id           uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    document_id       uuid        NOT NULL REFERENCES document_records(id) ON DELETE RESTRICT,
    document_party_id uuid        NOT NULL UNIQUE REFERENCES document_record_parties(id) ON DELETE RESTRICT,
    method            text        NOT NULL CHECK (method IN ('paper', 'in_person_click', 'provider')),
    signed_at         timestamptz NOT NULL DEFAULT clock_timestamp(),
    recorded_by       uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    provider_ref      text        CHECK (provider_ref IS NULL OR length(provider_ref) <= 200),
    evidence_key      text        CHECK (evidence_key IS NULL OR length(evidence_key) <= 200),
    witnessed_by      text        CHECK (witnessed_by IS NULL OR length(witnessed_by) <= 120),
    document_sha256   bytea       NOT NULL CHECK (length(document_sha256) = 32),
    CHECK (method <> 'provider' OR provider_ref IS NOT NULL)
);
CREATE INDEX document_signatures_document_idx ON document_signatures (document_id);

-- A link the customer opens, as an invoice's (0053).
CREATE TABLE document_record_links (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id         uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    document_id     uuid        NOT NULL REFERENCES document_records(id) ON DELETE RESTRICT,
    token_sha256    bytea       NOT NULL UNIQUE,
    created_by      uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at      timestamptz NOT NULL,
    revoked_at      timestamptz,
    revoked_by      uuid        REFERENCES users(id) ON DELETE RESTRICT,
    first_opened_at timestamptz,
    last_opened_at  timestamptz,
    opens           integer     NOT NULL DEFAULT 0,
    CHECK (expires_at > created_at),
    CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);
CREATE INDEX document_record_links_document_idx ON document_record_links (document_id);

-- How long each kind is kept before a person's erasure may blank it: a
-- setting the shop makes, as rows, the latest counting. With none, seven
-- years -- the bookkeeping law's period for what supports a sale or a
-- purchase -- until somebody who knows says otherwise (G2).
CREATE TABLE document_retention_settings (
    id      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id uuid        NOT NULL REFERENCES shops(id) ON DELETE RESTRICT,
    kind    text        NOT NULL CHECK (kind IN ('purchase_agreement', 'sales_contract', 'test_drive_agreement',
                                                 'consignment_agreement', 'rental_agreement')),
    years   integer     NOT NULL CHECK (years BETWEEN 1 AND 30),
    set_by  uuid        NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    set_at  timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX document_retention_settings_kind_idx ON document_retention_settings (shop_id, kind, set_at DESC);

DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['document_templates', 'document_template_versions', 'document_records',
                             'document_record_parties', 'document_signatures', 'document_record_links',
                             'document_retention_settings'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY shop_isolation ON %I
            USING (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)
            WITH CHECK (shop_id = nullif(current_setting('app.current_shop', true), '')::uuid)$p$, t);
        -- The default privileges (0018) give everything; revoke first, as
        -- 0071 found the hard way.
        EXECUTE format('REVOKE ALL ON %I FROM freesms_app', t);
        EXECUTE format('GRANT SELECT, INSERT ON %I TO freesms_app', t);
    END LOOP;
END;
$$;
GRANT UPDATE (name) ON document_templates TO freesms_app;
GRANT UPDATE (body, data, erased_at) ON document_records TO freesms_app;
GRANT UPDATE (name, org_number, address) ON document_record_parties TO freesms_app;
GRANT UPDATE (revoked_at, revoked_by, first_opened_at, last_opened_at, opens) ON document_record_links TO freesms_app;

-- A document is frozen by the database, as an invoice is (0007): nothing
-- changes it and nothing deletes it, whatever writes. The one change let
-- through is the erasure: what it said blanked, everything else as it was.
-- Its parties as named are frozen the same way, and blanked only on a
-- document so erased.
CREATE FUNCTION document_records_are_frozen() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a document is never deleted (%)', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
    IF TG_TABLE_NAME = 'document_records' THEN
        IF OLD.erased_at IS NULL AND NEW.erased_at IS NOT NULL AND NEW.body IS NULL AND NEW.data IS NULL THEN
            RETURN NEW;
        END IF;
    ELSIF EXISTS (SELECT 1 FROM document_records d WHERE d.id = OLD.document_id AND d.erased_at IS NOT NULL) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'a document is frozen once produced (%); a correction is a new one that supersedes it', OLD.id
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER document_records_are_frozen BEFORE UPDATE OR DELETE ON document_records
    FOR EACH ROW EXECUTE FUNCTION document_records_are_frozen();
CREATE TRIGGER document_records_are_frozen BEFORE UPDATE OR DELETE ON document_record_parties
    FOR EACH ROW EXECUTE FUNCTION document_records_are_frozen();

-- A signature is for the document as it is: of the hash it was given to
-- sign, and not of one a correction has replaced.
CREATE FUNCTION signatures_are_of_this_document() RETURNS trigger AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM document_records d JOIN document_record_parties p ON p.document_id = d.id
                    WHERE d.id = NEW.document_id AND p.id = NEW.document_party_id) THEN
        RAISE EXCEPTION 'that party is not on that document' USING ERRCODE = 'check_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM document_records WHERE supersedes_id = NEW.document_id) THEN
        RAISE EXCEPTION 'a superseded document is not signed (%)', NEW.document_id USING ERRCODE = 'restrict_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM document_records WHERE id = NEW.document_id AND content_sha256 = NEW.document_sha256) THEN
        RAISE EXCEPTION 'the signature is of something other than the document (%)', NEW.document_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER signatures_are_of_this_document BEFORE INSERT ON document_signatures
    FOR EACH ROW EXECUTE FUNCTION signatures_are_of_this_document();
CREATE FUNCTION signatures_stay() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'a signature is evidence and stays' USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER signatures_stay BEFORE UPDATE OR DELETE ON document_signatures
    FOR EACH ROW EXECUTE FUNCTION signatures_stay();

-- A template version is the words documents were made from, and stays them.
CREATE FUNCTION template_versions_stay() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'a template version is never changed; save the template to make the next one'
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER template_versions_stay BEFORE UPDATE OR DELETE ON document_template_versions
    FOR EACH ROW EXECUTE FUNCTION template_versions_stay();
