-- rollback: safe
-- A column with a default every earlier profile version meets and a CHECK
-- widened by one value the previous release never writes.
--
-- Winter tyres on a rental car (#233). A profile may require that a car handed
-- over in the winter period -- from 1 December to 31 March -- stands on a winter
-- or an all-season set. Off for every profile until the shop turns it on: whether
-- it must is the shop's rule. A handover that lacks it may be let go by whoever
-- runs the shop, with a reason, as a missing report or insurance is.
ALTER TABLE rental_profile_versions ADD COLUMN winter_tyres_required boolean NOT NULL DEFAULT false;
SELECT widen_text_check('rental_gap_overrides', 'rental_gap_overrides_code_check', 'code', ARRAY['winter_tyres']);
