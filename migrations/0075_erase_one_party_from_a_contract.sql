-- rollback: safe
-- Lets through two more changes to a document already erased, and grants
-- the columns they write. The previous release makes neither change, and
-- what it does make is let through as before.
--
-- A person's erasure from a contract past its retention (0072) blanked
-- every party on it, and kept its title and the scan of the signed paper
-- copy, both of which can name them. Now only the erased person's party
-- is blanked, which the trigger already allowed on an erased document; and
-- on such a document the title may be changed -- to take their name out of
-- it -- and a signature may lose its scan, and nothing else.
CREATE OR REPLACE FUNCTION document_records_are_frozen() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a document is never deleted (%)', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
    IF TG_TABLE_NAME = 'document_records' THEN
        IF OLD.erased_at IS NULL AND NEW.erased_at IS NOT NULL AND NEW.body IS NULL AND NEW.data IS NULL THEN
            RETURN NEW;
        END IF;
        -- Erased already, and another of its parties erased since: their
        -- name taken out of the title, everything else as it was.
        IF OLD.erased_at IS NOT NULL AND NEW.erased_at = OLD.erased_at AND NEW.body IS NULL AND NEW.data IS NULL
           AND NEW.content_sha256 = OLD.content_sha256 THEN
            RETURN NEW;
        END IF;
    ELSIF EXISTS (SELECT 1 FROM document_records d WHERE d.id = OLD.document_id AND d.erased_at IS NOT NULL) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'a document is frozen once produced (%); a correction is a new one that supersedes it', OLD.id
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

-- A signature is evidence and stays; on an erased document it loses the
-- scan, a picture of the whole contract, and keeps the rest: who, how,
-- when, recorded by whom, and the hash of what was signed.
CREATE OR REPLACE FUNCTION signatures_stay() RETURNS trigger AS $$
DECLARE
    kept document_signatures;
BEGIN
    IF TG_OP = 'UPDATE' AND EXISTS (SELECT 1 FROM document_records d WHERE d.id = OLD.document_id AND d.erased_at IS NOT NULL) THEN
        kept := OLD;
        kept.evidence_key := NULL;
        IF NEW IS NOT DISTINCT FROM kept THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'a signature is evidence and stays' USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

GRANT UPDATE (title) ON document_records TO freesms_app;
GRANT UPDATE (evidence_key) ON document_signatures TO freesms_app;
