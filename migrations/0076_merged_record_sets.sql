-- rollback: safe
-- Three new functions and three indexes, which the previous release does
-- not call and does not mind.
--
-- customer_now, vehicle_now and party_now (0068, 0069) answer what one
-- record is now, through any chain of merges. Asked of every row --
-- customer_now(i.customer_id) = $1 over every invoice -- they run a
-- recursive query per row and keep the planner from any index, so a read
-- grows with the shop's whole history. These answer the other way round,
-- once: every id that is read as c -- c itself and everything merged into
-- it, through any chain -- so a query asks x = ANY(the set) of an indexed
-- column. Empty when c has itself been merged away, or is not there: no x
-- has customer_now(x) = c then either, so the two forms agree.
CREATE FUNCTION customer_ids_read_as(c uuid) RETURNS uuid[] LANGUAGE sql STABLE AS $$
    WITH RECURSIVE down(id, depth) AS (
        SELECT id, 0 FROM customers WHERE id = c AND merged_into IS NULL
        UNION ALL
        SELECT k.id, down.depth + 1 FROM customers k JOIN down ON k.merged_into = down.id WHERE down.depth < 64
    )
    SELECT coalesce(array_agg(id), '{}') FROM down
$$;

CREATE FUNCTION vehicle_ids_read_as(v uuid) RETURNS uuid[] LANGUAGE sql STABLE AS $$
    WITH RECURSIVE down(id, depth) AS (
        SELECT id, 0 FROM vehicles WHERE id = v AND merged_into IS NULL
        UNION ALL
        SELECT k.id, down.depth + 1 FROM vehicles k JOIN down ON k.merged_into = down.id WHERE down.depth < 64
    )
    SELECT coalesce(array_agg(id), '{}') FROM down
$$;

CREATE FUNCTION party_ids_read_as(p uuid) RETURNS uuid[] LANGUAGE sql STABLE AS $$
    WITH RECURSIVE down(id, depth) AS (
        SELECT id, 0 FROM parties WHERE id = p AND merged_into IS NULL
        UNION ALL
        SELECT k.id, down.depth + 1 FROM parties k JOIN down ON k.merged_into = down.id WHERE down.depth < 64
    )
    SELECT coalesce(array_agg(id), '{}') FROM down
$$;

-- What the walk down follows.
CREATE INDEX customers_merged_into_idx ON customers (merged_into) WHERE merged_into IS NOT NULL;
CREATE INDEX vehicles_merged_into_idx ON vehicles (merged_into) WHERE merged_into IS NOT NULL;
CREATE INDEX parties_merged_into_idx ON parties (merged_into) WHERE merged_into IS NOT NULL;
