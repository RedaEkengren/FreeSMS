-- The shop's hourly rate.
--
-- A stored time is minutes; a line needs money. Without a rate the library can
-- only fill in half a line, and the half it leaves is the one that gets typed
-- wrong at five to five.
--
-- Zero means "not set", and a suggestion then fills in the hours and leaves
-- the price blank rather than quietly quoting nothing.
ALTER TABLE shops ADD COLUMN labour_rate_minor bigint NOT NULL DEFAULT 0
    CHECK (labour_rate_minor >= 0);

COMMENT ON COLUMN shops.labour_rate_minor IS
    'Hourly labour rate in minor units, excluding VAT. 0 means not configured.';
