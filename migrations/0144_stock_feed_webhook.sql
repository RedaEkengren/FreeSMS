-- rollback: safe
-- A nullable column the previous release neither reads nor writes. It inserts
-- its settings rows without it, so a webhook set under this release is not
-- carried through a rollback; the feed itself is, and a site that polls with
-- changes-since keeps working.
--
-- Where a shop's site is told that its stock changed (#183): an address the
-- shop chooses, called through the outbox with a ping that carries no car
-- and no price, only that something changed. The site then asks the feed.
ALTER TABLE stock_feed_settings ADD COLUMN webhook_url text
    CHECK (webhook_url IS NULL OR (webhook_url ~ '^https://[^[:space:]]+$' AND length(webhook_url) <= 500));
