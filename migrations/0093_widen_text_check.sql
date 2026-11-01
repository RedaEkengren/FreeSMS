-- rollback: safe
-- A new function and nothing else; the previous release never calls it.
--
-- Car sales and rental are built on branches of their own, and each adds
-- values to the same CHECK lists of Core's tables: the subjects an audit
-- event may have, the sources of an odometer reading. Writing the whole
-- list out again, as one migration used to, takes away whatever the other
-- branch added, depending on the order they were applied in. This adds to
-- the list as it stands instead.
--
-- It reads the allowed values in either form pg_get_constraintdef prints
-- them -- 'a'::text, 'b'::text from an IN list or an ARRAY[...], and
-- '{a,b}'::text[] from an array literal -- adds only those missing, and
-- writes the constraint back as col = ANY (ARRAY[...]::text[]), which is
-- printed in the first form again. Nothing missing, nothing is rewritten.
-- A constraint it cannot read -- not there, about another column, with no
-- list in it -- is refused: a list read as nothing would otherwise be
-- written back as ANY (NULL), which allows every value.
--
-- For a constraint that is a list of values and nothing else: anything
-- more in it would not survive being written back.
CREATE FUNCTION widen_text_check(tbl regclass, con name, col name, add text[]) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    def    text;
    vals   text[];
    lit    text[];
    wanted text[];
BEGIN
    SELECT pg_get_constraintdef(c.oid) INTO def
      FROM pg_constraint c
     WHERE c.conrelid = tbl AND c.conname = con AND c.contype = 'c';
    IF def IS NULL THEN
        RAISE EXCEPTION 'widen_text_check: % has no check constraint %', tbl, con;
    END IF;
    IF def !~ ('\m' || col || '\M') THEN
        RAISE EXCEPTION 'widen_text_check: % on % is not about %', con, tbl, col;
    END IF;
    -- 'a'::text, as an IN list or an ARRAY[...] prints.
    SELECT array_agg(m[1]) INTO vals
      FROM regexp_matches(def, '''((?:[^'']|'''')*)''::text(?!\[)', 'g') m;
    -- '{a,b}'::text[], as an array literal prints.
    FOR lit IN SELECT m FROM regexp_matches(def, '''((?:[^'']|'''')*)''::text\[\]', 'g') m LOOP
        vals := coalesce(vals, '{}') || replace(lit[1], '''''', '''')::text[];
    END LOOP;
    IF vals IS NULL OR cardinality(vals) = 0 THEN
        RAISE EXCEPTION 'widen_text_check: no allowed values could be read from % on %: %', con, tbl, def;
    END IF;
    vals := ARRAY(SELECT DISTINCT unnest(vals) ORDER BY 1);
    wanted := ARRAY(SELECT DISTINCT a FROM unnest(add) a WHERE a IS NOT NULL AND NOT a = ANY (vals) ORDER BY 1);
    IF cardinality(wanted) = 0 THEN
        RETURN;
    END IF;
    vals := vals || wanted;
    EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', tbl, con);
    EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I CHECK (%I = ANY (ARRAY[%s]::text[]))', tbl, con, col,
                   (SELECT string_agg(quote_literal(v), ', ') FROM unnest(vals) v));
END;
$$;
