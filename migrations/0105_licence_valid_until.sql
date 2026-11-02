-- rollback: safe
-- A new nullable column. The previous release writes checks without it,
-- and this release reads a check without it as one that does not pass.
--
-- Until when the licence a check passed is valid, so the check is asked
-- again against the rental as it is when it is handed over: extended past
-- the licence's end, it no longer passes (#193). A date, not an identifier:
-- what the licence says otherwise stays in the identity record.
ALTER TABLE rental_licence_checks ADD COLUMN valid_until date;
