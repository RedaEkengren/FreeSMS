-- rollback: safe
-- A grant only. The previous release never updates the column.
--
-- An extension adds the kilometres the plan includes for the days it adds
-- (#192), so the allowance agreed on a booking grows with it; each change is
-- the extension's event, as its period's is.
GRANT UPDATE (included_km) ON rental_bookings TO freesms_app;
