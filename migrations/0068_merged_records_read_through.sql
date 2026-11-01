-- rollback: safe
-- Two new functions, which nothing in the previous release calls.
--
-- What a merged-away record is now. Merging two customers moves their jobs,
-- ownership and contacts (0049), but not what a frozen document names: an
-- issued invoice's customer_id (0065) and a finished protocol's vehicle_id
-- (0066) are immutable by trigger, and rewriting them on a merge would be the
-- exception the triggers exist to refuse. So the documents keep the record
-- they were made against, and a reader follows the merge: customer_now(id)
-- is the customer id was merged into, through any chain of merges, or id
-- itself. vehicle_now is the same for vehicles.
--
-- Read as the caller, so row level security applies inside them as it does
-- to the query that calls them. A chain is never longer than the merges a
-- shop has done; the depth limit is only a guard against a cycle the
-- constraints should already make impossible.
CREATE FUNCTION customer_now(c uuid) RETURNS uuid LANGUAGE sql STABLE AS $$
    WITH RECURSIVE up(id, next, depth) AS (
        SELECT id, merged_into, 0 FROM customers WHERE id = c
        UNION ALL
        SELECT k.id, k.merged_into, up.depth + 1 FROM customers k JOIN up ON k.id = up.next WHERE up.depth < 64
    )
    SELECT id FROM up ORDER BY depth DESC LIMIT 1
$$;

CREATE FUNCTION vehicle_now(v uuid) RETURNS uuid LANGUAGE sql STABLE AS $$
    WITH RECURSIVE up(id, next, depth) AS (
        SELECT id, merged_into, 0 FROM vehicles WHERE id = v
        UNION ALL
        SELECT k.id, k.merged_into, up.depth + 1 FROM vehicles k JOIN up ON k.id = up.next WHERE up.depth < 64
    )
    SELECT id FROM up ORDER BY depth DESC LIMIT 1
$$;
