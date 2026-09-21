-- The refusal message, in English a person would write.
--
-- It said "an issued invoice cannot be update", because it interpolated the
-- trigger operation directly. Small, and worth a migration of its own: this is
-- the sentence somebody reads at five to five when they are trying to correct
-- a document, and it is the sentence that has to send them to a credit note
-- instead of to a support call.
--
-- 0007 cannot simply be edited. An applied migration whose content changes is
-- refused at startup, on purpose -- it means the database and the repository
-- disagree about the schema. Correcting one is a new migration, always.
CREATE OR REPLACE FUNCTION invoices_are_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION
        'an issued invoice cannot be changed or deleted (% on %, invoice %); correct it with a credit note',
        TG_OP, TG_TABLE_NAME, coalesce(OLD.id::text, '?')
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
