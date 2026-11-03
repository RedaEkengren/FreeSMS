-- rollback: safe
-- Lists widened and nothing else: the previous release writes none of the
-- new values, and reads a reservation's terms -- a kind it does not know --
-- as a document it has no heading for.
--
-- A reservation's terms are a frozen document given to the customer
-- (#174): until when the car is held, whether the deposit is refunded and
-- on what conditions. The shipped template is a draft until the legal
-- review (#221) has read it.
SELECT widen_text_check('document_templates', 'document_templates_kind_check', 'kind', ARRAY['reservation_terms']);
SELECT widen_text_check('document_records', 'document_records_kind_check', 'kind', ARRAY['reservation_terms']);
SELECT widen_text_check('document_records', 'document_records_subject_kind_check', 'subject_kind', ARRAY['reservation']);
SELECT widen_text_check('document_retention_settings', 'document_retention_settings_kind_check', 'kind', ARRAY['reservation_terms']);
SELECT widen_text_check('document_templates', 'document_templates_shipped_check', 'shipped', ARRAY['reservation_terms']);
