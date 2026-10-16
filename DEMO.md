# A practice copy

A workshop should try a whole day before its first customer. A practice copy is
a second FreeSMS with its own database and its own port, started beside the
real one and thrown away when you are done. Nothing in it touches the real
workshop, and nothing from the real one is in it: it starts empty.

```sh
deploy/demo.sh          # starts it, prints the address
deploy/demo.sh down     # removes it and everything in it
```

Then, in the practice copy:

1. Set it up as the owner (the page that opens first).
2. Under **Import**, upload the files in `examples/import/` in this order:
   `customers.csv`, `vehicles.csv`, `parts.csv`, `stock.csv`. Each is shown to
   you before anything is written.
3. Take in a car, price it, invoice it and close the day. The invoices there
   are practice invoices in a practice database; they are never copied to, and
   never take a number from, the real workshop's series.

Do not use the practice copy for anything real: its database password is
published in the script, and it is deleted without a backup.

The same files, with your own data in place of the examples, are what you
upload to the real workshop. See `IMPORT.md`.
