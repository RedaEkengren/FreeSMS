-- rollback: safe
-- A new function, and the view of 0090 rewritten to read through it with
-- the same columns and the same rows. The previous release reads the view
-- as before, and its session reads the tables directly, which still works.
--
-- What is opened for a technician (#225) was written out twice: once in
-- the view the settings page reads (technician_fields_open, 0090), and
-- once in the session's lookup, which needs it for one person on every
-- request. Two copies of a rule drift apart, and the one that drifted
-- decides what a technician is shown. Now the rule is this function, and
-- both read it: the view for everybody, the session for one person.
--
-- What is open now: the shop's profile for every technician (user_id
-- null), and each person's exceptions not ended and not past their end --
-- every person's, or only_user's when it is given.
CREATE FUNCTION technician_fields_open_rows(only_user uuid)
RETURNS TABLE (shop_id uuid, user_id uuid, field text) LANGUAGE sql STABLE AS $$
    SELECT p.shop_id, NULL::uuid, p.field
      FROM (SELECT DISTINCT ON (c.shop_id, c.field) c.shop_id, c.field, c.shown
              FROM technician_view_changes c ORDER BY c.shop_id, c.field, c.seq DESC) p
     WHERE p.shown
    UNION ALL
    SELECT e.shop_id, e.user_id, e.field
      FROM (SELECT DISTINCT ON (x.user_id, x.field) x.shop_id, x.user_id, x.field, x.granted, x.until
              FROM technician_view_exceptions x
             WHERE only_user IS NULL OR x.user_id = only_user
             ORDER BY x.user_id, x.field, x.seq DESC) e
     WHERE e.granted AND (e.until IS NULL OR e.until > clock_timestamp())
$$;

CREATE OR REPLACE VIEW technician_fields_open WITH (security_invoker = true) AS
    SELECT shop_id, user_id, field FROM technician_fields_open_rows(NULL);
