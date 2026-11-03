-- rollback: safe
-- Fills a nullable column the previous release already writes and reads;
-- no table, column or constraint changes.
--
-- A booking freezes the rate plan it was agreed under (plan_id), and an
-- extension adds kilometres by that plan. Every booking the code makes
-- names it, but the column is nullable, and a booking without it would be
-- extended under whichever plan prices the unit today. Where the booking
-- does not say, its own first lines do: the rent lines written when it
-- was agreed carry the plan that priced them. A booking whose first rent
-- lines name one plan gets that plan; one whose do not stays without, and
-- an extension then leaves its kilometres as agreed.
UPDATE rental_bookings b SET plan_id = first.plan_id
FROM (
    SELECT l.booking_id, min(l.plan_id::text)::uuid AS plan_id
    FROM rental_booking_lines l
    WHERE l.kind = 'rent' AND l.plan_id IS NOT NULL
      AND l.event_id = (SELECT f.event_id FROM rental_booking_lines f WHERE f.booking_id = l.booking_id ORDER BY f.position LIMIT 1)
    GROUP BY l.booking_id
    HAVING count(DISTINCT l.plan_id) = 1
) first
WHERE b.id = first.booking_id AND b.plan_id IS NULL;
