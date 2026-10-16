# What to try with a real workshop

Run these on the devices people will really use, in Swedish and once in
English, before and during a pilot. The automated browser tests cover the same
ground in a fixed order (`e2e/tests`); a person finds what they cannot, such as
a screen too small for a thumb, a sentence that reads oddly to a mechanic, or a
step nobody explained. Write down anything that made somebody stop and ask.

Use a practice copy first (`DEMO.md`).

## Reception, on the counter computer

| Task | Done when |
|---|---|
| Take a car in, with a customer who is new | The job has a customer and a complaint |
| Take a car in that is already known | Its owner and history were filled in |
| Price the work and put the quote to the customer | The customer's yes is on the job |
| Add a line after the yes | It asks for a new yes before it counts |
| Tell the customer the car is ready | It is written down that they were told |
| Invoice, record a payment, credit a line | The balance is right and no document was edited |
| Close the day | The figures match the bank, or the difference has a reason |

## Technician, on a phone

| Task | Done when |
|---|---|
| Find today's jobs and start the clock | The clock runs, and starting another stops it |
| Tick off steps; mark one blocked and say why | The counter's queue shows it |
| Ask for a part; take one out | The parts desk sees it; stock moves once |
| Report a finding with a photo | The counter can price it; the technician saw no customer details |
| Work with the connection dropped for a minute | The page says it is holding the work, and it arrives once |
| Say the car is ready | It refuses while work or a final check is open |

## Owner

| Task | Done when |
|---|---|
| Work through Getting started | It says ready, and every "required" item has a name or a value |
| Import customers, cars, parts and opening stock | The preview was read, and a repeat changed nothing |
| Add a person with the right role | They cannot see more than their role does |
| Export to the accountant | The accountant confirms the file read |
| Restore a backup into a practice copy | Yesterday's invoice is there (`DEPLOY.md`) |

## Every device

- Every control can be reached and used with a thumb at 360 px wide; nothing
  scrolls sideways.
- Tab reaches "Skip to the content" first and every stop shows where it is.
- A screen reader announces an error, and that the page was updated by someone
  else.
- Dark and light are both readable; status is always in words.
- A long Swedish word does not push a button off the screen.
