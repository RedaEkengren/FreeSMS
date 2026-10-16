# Importing what a shop already has

The owner starts from **Import** in the navigation. A file is read and shown
before anything is written; applying it is all or nothing.

Do the files in this order, because each one finds what an earlier one made:

1. **customers**
2. **vehicles** (find their owner by the customer's reference)
3. **parts**
4. **stock** (opening stock; finds its part by number)

Repair history is not imported. It would have to be labelled as someone else's
record and could not carry invoice numbers or financial documents.

## The file

CSV, up to 5 MB and 20 000 rows. Semicolons, commas or tabs. UTF-8 (with or
without a byte order mark) or Windows-1252. A decimal comma or a decimal point;
a space as the thousands separator (`1 234,50`); at most the decimals the field
has (two for money, three for quantities) — more is an error, never rounded.
Dates are `2026-01-31`. The first line is the column names. Templates with an
example row can be downloaded from the Import page.

A cell that begins with `=`, `+`, `@` (or a minus followed by a letter) is
refused on its row: a spreadsheet would run it if the data were exported again.

## Columns

Swedish headers from the usual packages are understood (`kundnr`, `namn`,
`telefon`, `regnr`, `artikelnummer`, `inpris`, `antal` …); a column that is not
recognised is reported rather than dropped.

| file | columns (required in bold) |
|---|---|
| customers | **ref**, type (`private`/`company`), **name**, contact, org_number, vat_number, phone, email, address, address2, postal_code, city |
| vehicles | **registration**, vin, make, model, year, engine, owner_ref, odometer_km, odometer_date |
| parts | **number**, **name**, unit (`each`, `litre`, `metre`, `kilogram`, `hour`; `st`, `liter`, `kg`, `tim` …), cost, price, minimum, location |
| stock | **part_number**, **quantity**, **as_of**, unit_cost |

## What the preview says

Each row is *would be created*, *would be filled in*, *nothing to add* or
*cannot be imported* with the reason. A new customer who shares a company
number, email or telephone number with someone already here (or with an earlier
row of the file) is listed with the candidates, and **applying is refused until
each such row is answered**: create it, treat it as the same person, or leave
it out.

Records that already exist are only **filled in where they are empty**. A
value that differs is reported and left as it is.

## Identity and retries

- A file is identified by its contents. Uploading the same file again opens the
  same batch; applying a batch twice does nothing the second time.
- Customers are found again by `ref`, vehicles by registration, parts by
  number, opening stock by part and day. A corrected file therefore fills in
  what the first one made instead of making it again.
- Opening stock is a dated entry in the stock ledger (`counted`, noon on the
  stated day, shop time) that names the batch it came from. The same part on
  the same day is never added twice; a different quantity for it is an error
  to be corrected with a stocktake.

## Atomicity and recovery

Applying runs in one transaction. Rows that cannot be imported are never
written; to apply the others the person has to say so. A failure while writing
rolls the whole batch back and leaves it previewed, so it can be applied again
after the cause is fixed. A vehicle whose owner reference is unknown is created
without an owner and the report says so.

There is no automatic undo for a batch that was applied. Every record a batch
created is findable through `import_refs`, every value it filled in is in the
record's change log with the file's name as the reason, and opening stock is
reversed with a stocktake like any other balance.

## Personal data

The file is read in memory and never written to disk. While a batch waits, its
rows hold what the file said; they are wiped when the batch is applied or
discarded, and by the daily retention sweep a week after upload. The batch's
own record (file name, who, when, counts) and the reference table stay.
Contents are never logged.
