package workshop_test

import "time"

// The tests' clock is the seeded shop's: Europe/Stockholm. A test passes
// time.Now() where a person types today's date, and a form reads that date
// on the shop's clock. On a runner whose clock is UTC, between midnight and
// one in the morning Swedish time, time.Now() was yesterday's date while
// the shop's day -- a sale, the day's figures -- was today's, and costs,
// payments and margins fell on the wrong side of it (CI, 2026-11-03
// 23:45 UTC). The product reads the shop's day in the shop's zone; this
// makes the tests' "today" the same day.
func init() { time.Local = shopZone() }
