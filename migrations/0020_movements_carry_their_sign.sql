-- The stock ledger's premise is that the sum of the movements is the truth.
-- The sign is what makes the sum work: on_hand is a plain sum(quantity), so a
-- 'consumed' row written positive raises the figure on the shelf instead of
-- lowering it, and nothing complains.
--
-- The application has always written the right sign. That is not the point.
-- The column is the record and the code is this year's way of writing to it;
-- a hand-written row, a restored dump, or an import next year all reach the
-- table without going past a switch statement in Go. The table already ties
-- reason to written_off, so the pattern is established here.
--
-- counted is the one kind left free. A stocktake says what is on the shelf and
-- the movement is the difference, which is negative when somebody has taken
-- something without booking it out and positive when a part turns up behind
-- another one. Forcing a direction on it would make the honest entry
-- impossible to record.
ALTER TABLE stock_movements
    ADD CONSTRAINT stock_movements_sign_matches_kind CHECK (
        CASE kind
            -- Onto the shelf.
            WHEN 'received'   THEN quantity > 0
            -- Off it.
            WHEN 'consumed'   THEN quantity < 0
            WHEN 'returned'   THEN quantity < 0
            WHEN 'written_off' THEN quantity < 0
            -- Promised to a job, and released again. Reserved is its own
            -- running total, not part of on_hand, and the two have to cancel.
            WHEN 'reserved'   THEN quantity > 0
            WHEN 'unreserved' THEN quantity < 0
            -- A stocktake difference goes either way.
            WHEN 'counted'    THEN true
            -- A kind nobody planned for. The kind constraint refuses it
            -- already; this must not be the thing that lets it through.
            ELSE false
        END
    );
