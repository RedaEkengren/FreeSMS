# The stock feed

A dealer's website reads the cars for sale from FreeSMS instead of having
them typed in twice. The feed is part of car sales (#183): without the
Dealer module it answers as a page that does not exist.

The owner turns it on under **Cars in stock → Your website's feed**:
published or not, open to anyone or behind a token, and whether reserved
cars are included. A token is shown once, when it is made; FreeSMS keeps
only its SHA-256. Making a new one stops the old one working.

## Requests

    GET /feed/v1/stock.json
    Authorization: Bearer <token>          (or ?token=<token>)

    GET /feed/v1/stock.json?since=2026-11-03T12:00:00Z

`since` returns only the cars whose details, price or photographs changed
after that moment -- a photograph added, taken away, or moved to the front
included. A car that leaves the feed -- reserved, sold, delivered
or taken back into preparation -- is simply no longer in it, from the
transaction that changed its status: compare the ids you hold with a full
fetch to remove them.

Every response carries an `ETag`; send it back as `If-None-Match` and an
unchanged feed answers `304 Not Modified`. `Last-Modified` is the newest
change in it.

A feed not published, or asked for without its token or with a wrong one,
answers `404`, the same as a path that does not exist.

The feed may be read about ten times a second, all callers together, with
room for fifty at once. Past that it answers `429 Too Many Requests` with a
`Retry-After` header in seconds; send `If-None-Match`, or use `since`, and a
site is nowhere near it. The photographs are not counted: they are cacheable.

## The document

```json
{
  "version": 1,
  "cars": [
    {
      "id": "6f0c…",
      "make": "Volvo",
      "model": "V70",
      "variant": "D4 Momentum",
      "model_year": 2018,
      "odometer_km": 123600,
      "fuel": "Diesel",
      "gearbox": "Automatic",
      "colour": "Silver",
      "price_minor_including_vat": 12990000,
      "vat_deductible_for_business": false,
      "equipment": ["Tow bar", "Heater"],
      "description": "…",
      "reserved": false,
      "photos": ["2b1e…", "9c4a…"],
      "changed_at": "2026-11-03T12:34:56Z"
    }
  ]
}
```

| Field | Meaning |
|---|---|
| `id` | The car's stable public id for as long as it is in stock this time. A car bought back later is a new id. |
| `price_minor_including_vat` | The consumer price including VAT, in öre: divide by 100 for kronor. Absent when no price is set. |
| `vat_deductible_for_business` | The shop offers the car with ordinary VAT, which a business buyer may deduct. |
| `photos` | Public ids, the cover first. Fetch each from `/feed/v1/photos/<id>` with the same token. |
| `changed_at` | When anything in this car's entry last changed. |

The fields are an allow-list: a field reaches the feed only by being added
to `workshop.FeedCar`. What a car was bought for, its costs, its seller, a
consigned car's principal and anything written about it inside the shop
never do, and a test (`TestTheStockFeedShowsOnlyWhatIsForTheWebsite`)
checks the feed for them.

## Photographs

    GET /feed/v1/photos/<id>

A JPEG, cacheable by anyone for an hour. A photograph removed from the car,
or of a car no longer in the feed, answers `404`.

## Webhook

The owner may give an address, https only, under the same heading. FreeSMS then
calls it with a `POST` when stock changes: a car entering or leaving the feed,
a new listing, a photograph added, removed or moved to the front.

    {"event": "stock.changed", "since": "2026-11-03T12:00:00Z"}

The call carries no car, price or photograph: ask the feed with the same
token, `?since=` the value given, for what changed. Changes are gathered in
windows of ten seconds and one call is made for each window that had any,
when it has closed, so uploading eighty photographs is one call. The call goes
through the outbox: a site that answers `5xx`, `429` or not at all is called
again with back-off and the same `Idempotency-Key`, and an owner sees one that
keeps failing. Any other `4xx` is not retried. Redirects are not followed, and
an address inside the server's own network (loopback, private, link-local) is
never called. The call is not signed, so treat it as a nudge to read the feed
and nothing more.

## Not yet


- A financing example, which needs the information #184 requires beside it.
