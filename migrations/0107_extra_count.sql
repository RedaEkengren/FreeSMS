-- rollback: safe
-- A new nullable column. The previous release writes lines without it, and
-- this release reads such a line as it read them before.
--
-- How many of an extra a booking took -- two child seats -- kept on its
-- line, so an extension charges the same number for the days it adds
-- (#192), whatever the wall clock did to the days in between.
ALTER TABLE rental_booking_lines ADD COLUMN extra_count integer CHECK (extra_count IS NULL OR extra_count > 0);
