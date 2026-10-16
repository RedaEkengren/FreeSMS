-- rollback: restore
-- Messages no longer all belong to a job. The previous release reads every
-- queued message as having one, and would fail on the first that has none, so
-- it must not run against this schema while a booking message is waiting.
--
-- A booking exists before a work order does, and customers are told about it
-- before the car comes. A confirmation, a reminder, a move and a cancellation
-- go through the same queue as everything else -- the same providers, quiet
-- hours, retries and delivery reports -- without a job being invented to hang
-- them on.

ALTER TABLE outbound_messages ALTER COLUMN work_order_id DROP NOT NULL;
ALTER TABLE outbound_messages
    ADD COLUMN booking_id uuid REFERENCES bookings(id) ON DELETE RESTRICT,
    -- The time the booking had when this was queued. If it has another time
    -- when the message is about to go, the message is stale and is not sent.
    ADD COLUMN valid_for_starts timestamptz,
    ADD CONSTRAINT outbound_messages_about_something CHECK (work_order_id IS NOT NULL OR booking_id IS NOT NULL);
CREATE INDEX outbound_messages_booking_idx ON outbound_messages (booking_id) WHERE booking_id IS NOT NULL;

ALTER TABLE outbound_messages DROP CONSTRAINT IF EXISTS outbound_messages_about_check;
ALTER TABLE outbound_messages ADD CONSTRAINT outbound_messages_about_check
    CHECK (about IN ('ready', 'answer', 'waiting_part', 'invoice', 'reminder',
                     'booking_confirm', 'booking_reminder', 'booking_moved', 'booking_cancelled'));

-- The contact on a booking can be corrected, and a confirmation can be given
-- by hand when nothing could be sent.
ALTER TABLE booking_events DROP CONSTRAINT IF EXISTS booking_events_event_check;
ALTER TABLE booking_events ADD CONSTRAINT booking_events_event_check
    CHECK (event IN ('booked', 'moved', 'cancelled', 'arrived', 'no_show', 'contact_corrected', 'confirmed_by_hand'));
