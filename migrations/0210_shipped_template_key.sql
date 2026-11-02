-- rollback: safe
-- A new column on Core's document_templates, which the previous release
-- neither reads nor writes.
--
-- Which template is the one FreeSMS shipped for a kind -- the sales
-- contract (#175), the test-drive agreement (#173) -- by a key that stays
-- whatever the template is called or the shop's language becomes, not by
-- its name. One per kind and shop. Those already made are found by the
-- names they were made with.
ALTER TABLE document_templates ADD COLUMN shipped text CHECK (shipped IS NULL OR shipped IN ('sales_contract', 'test_drive_agreement'));
CREATE UNIQUE INDEX document_templates_shipped_idx ON document_templates (shop_id, shipped) WHERE shipped IS NOT NULL;

UPDATE document_templates t SET shipped = t.kind
 WHERE t.shipped IS NULL
   AND ((t.kind = 'sales_contract' AND t.name IN ('Sales contract (draft, needs legal review)',
                                                  'Köpeavtal (utkast, behöver juridisk granskning)'))
     OR (t.kind = 'test_drive_agreement' AND t.name IN ('Test-drive agreement (needs legal review)',
                                                        'Provkörningsavtal (behöver juridisk granskning)')))
   AND t.id = (SELECT min(u.id::text)::uuid FROM document_templates u
                WHERE u.shop_id = t.shop_id AND u.kind = t.kind AND u.name = t.name);
