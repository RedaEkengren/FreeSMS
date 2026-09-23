-- Which language a person reads.
--
-- Per user, falling back to the shop's, falling back to English. A workshop
-- with one Polish technician and three Swedish ones is ordinary, and making
-- them all read the same language because the shop has one setting is the kind
-- of small indignity software imposes without noticing.
ALTER TABLE users ADD COLUMN locale text;

COMMENT ON COLUMN users.locale IS
    'BCP 47 tag, or NULL to follow the shop.';
