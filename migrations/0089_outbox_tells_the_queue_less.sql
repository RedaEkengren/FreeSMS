-- rollback: safe
-- A trigger only, no table or column: the previous release's outbox rows
-- notify less, and what it listens for it still hears when the queue
-- changes.
--
-- 0088 told the one action queue on every outbox event: every attempt,
-- failure and retry of a call made every open home page and list re-read
-- itself, though the queue shows a call only once it is given up or is a
-- task for a person, and only to whoever runs the shop. Now an event
-- tells it only when a call enters one of those or leaves it -- a person
-- asks for it again, cancels it or does it by hand. Events are only ever
-- inserted (0080), so the trigger is on insert alone.
CREATE FUNCTION notify_outbox_queue() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.event IN ('given_up', 'manual_task')
       OR (SELECT p.event FROM outbox_events p
            WHERE p.outbox_id = NEW.outbox_id AND p.seq < NEW.seq
            ORDER BY p.seq DESC LIMIT 1) IN ('given_up', 'manual_task') THEN
        PERFORM pg_notify('freesms_changes', NEW.shop_id || ' actions');
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER notify_change ON outbox_events;
CREATE TRIGGER notify_change AFTER INSERT ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION notify_outbox_queue();
