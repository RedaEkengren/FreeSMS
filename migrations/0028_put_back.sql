-- rollback: safe
-- The kind is allowed and counted here, and nothing writes it yet: the
-- release that takes parts out to a job and puts them back comes after this
-- one. Rolled back from that release to this, the shelf still reads right;
-- rolled back from this one, there are no such rows to misread.
--
-- A part taken out to a job and not needed after all goes back on the shelf.
-- Not 'returned', which is to the supplier, and not a positive 'consumed',
-- which the sign of every consumption forbids: its own kind, onto the shelf.
ALTER TABLE stock_movements DROP CONSTRAINT stock_movements_kind_check;
ALTER TABLE stock_movements ADD CONSTRAINT stock_movements_kind_check CHECK (kind IN (
    'received', 'reserved', 'unreserved', 'consumed', 'returned', 'written_off', 'counted',
    'put_back'));

ALTER TABLE stock_movements DROP CONSTRAINT stock_movements_sign_matches_kind;
ALTER TABLE stock_movements ADD CONSTRAINT stock_movements_sign_matches_kind CHECK (
    CASE kind
        -- Onto the shelf.
        WHEN 'received'    THEN quantity > 0
        WHEN 'put_back'    THEN quantity > 0
        -- Off it.
        WHEN 'consumed'    THEN quantity < 0
        WHEN 'returned'    THEN quantity < 0
        WHEN 'written_off' THEN quantity < 0
        -- Promised to a job, and released again.
        WHEN 'reserved'    THEN quantity > 0
        WHEN 'unreserved'  THEN quantity < 0
        -- A stocktake difference goes either way.
        WHEN 'counted'     THEN true
        ELSE false
    END
);
