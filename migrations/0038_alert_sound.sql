-- rollback: safe
-- A nullable-free column with a default; the previous release never reads it.
--
-- Whether a person wants a sound, and a buzz on a phone, when something new
-- is waiting for them. Off unless they turn it on: a workshop is loud
-- enough, and a counter machine beeping for whoever last signed in is
-- nobody's choice.
ALTER TABLE users ADD COLUMN alert_sound boolean NOT NULL DEFAULT false;
