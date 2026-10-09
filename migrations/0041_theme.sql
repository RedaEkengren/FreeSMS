-- rollback: safe
-- A nullable column the previous release never reads.
--
-- Light or dark, by the person's own choice. Empty is "follow the device",
-- which is what everybody had before there was a choice.
ALTER TABLE users ADD COLUMN theme text CHECK (theme IN ('light', 'dark'));
