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
after that moment. A car that leaves the feed -- reserved, sold, delivered
or taken back into preparation -- is simply no longer in it, from the
transaction that changed its status: compare the ids you hold with a full
fetch to remove them.

Every response carries an `ETag`; send it back as `If-None-Match` and an
unchanged feed answers `304 Not Modified`. `Last-Modified` is the newest
change in it.

A feed not published, or asked for without its token or with a wrong one,
answers `404`, the same as a path that does not exist.

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

## Not yet

- A rate limit of its own.
- A webhook when the feed changes; poll with `since` instead.
- A financing example, which needs the information #184 requires beside it.
