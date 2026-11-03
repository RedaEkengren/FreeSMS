-- rollback: safe
-- Takes privileges away from the application that no release uses: every
-- statement the previous release runs on these tables is one still granted.
--
-- 0067, 0069 and 0070 granted the application what each table needs --
-- SELECT and INSERT, and UPDATE of the columns that may change -- and said
-- that this kept the rows from being changed or deleted. It did not: the
-- default privileges (0018) give the application SELECT, INSERT, UPDATE and
-- DELETE on every new table the moment it is made, and a GRANT only adds to
-- that. The tables before them revoke first (0048, 0063, ...); these did
-- not. A party could be deleted, moved to another customer; a removal's
-- record rewritten; a block deleted rather than released.
REVOKE ALL ON protocol_photo_removals, parties, party_roles, resources, resource_blocks, resource_block_changes
    FROM freesms_app;

GRANT SELECT, INSERT ON protocol_photo_removals TO freesms_app;

GRANT SELECT, INSERT ON parties TO freesms_app;
GRANT UPDATE (company_name, org_number, address_line1, address_line2, postal_code, city, country,
              revision, merged_into, merged_at) ON parties TO freesms_app;
GRANT SELECT, INSERT ON party_roles TO freesms_app;
GRANT UPDATE (valid_to, ended_by) ON party_roles TO freesms_app;

GRANT SELECT, INSERT ON resources TO freesms_app;
GRANT UPDATE (name, retired_at, retired_by) ON resources TO freesms_app;
GRANT SELECT, INSERT ON resource_blocks TO freesms_app;
GRANT UPDATE (status, period, ended_at, ended_by) ON resource_blocks TO freesms_app;
GRANT SELECT, INSERT ON resource_block_changes TO freesms_app;
