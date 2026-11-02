-- rollback: safe
-- A column with a default the previous release's inserts take, two
-- columns filled from rows that are there and then required, and a
-- unique constraint widened by them. The previous release reads neither
-- column; a version it writes takes the insurance rule on, the safe
-- reading; an override it writes names no period and is refused -- a
-- wrong sentence at worst, while the data stays right.
--
-- Whether a profile requires the insurance for rental use (#202): a rule
-- of its own, as the report is. The shipped equipment profile's versions
-- do not require it, as a tent is no vehicle; everything else does.
ALTER TABLE rental_profile_versions ADD COLUMN insurance_required boolean NOT NULL DEFAULT true;
UPDATE rental_profile_versions v SET insurance_required = false
FROM rental_profiles p WHERE p.id = v.profile_id AND p.base = 'equipment';

-- A handover gap let go is let go for the rental as it was then: its
-- booked start and end. Moved or extended, it is asked again. One let go
-- before this keeps the booking's period now.
ALTER TABLE rental_gap_overrides ADD COLUMN starts_at timestamptz, ADD COLUMN ends_at timestamptz;
UPDATE rental_gap_overrides o SET starts_at = b.starts_at, ends_at = b.ends_at
FROM rental_bookings b WHERE b.id = o.booking_id;
ALTER TABLE rental_gap_overrides ALTER COLUMN starts_at SET NOT NULL, ALTER COLUMN ends_at SET NOT NULL;
ALTER TABLE rental_gap_overrides DROP CONSTRAINT rental_gap_overrides_booking_id_code_key;
ALTER TABLE rental_gap_overrides ADD CONSTRAINT rental_gap_overrides_period_key UNIQUE (booking_id, code, starts_at, ends_at);
