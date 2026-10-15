import { test, expect, Page } from '@playwright/test';
import { tech, desk, signIn, signOut, shopClock, getApproved, agreeAtTheCounter } from './helpers';

// Taking a car in, and the jobs that follow from it: from a booking all the
// way to closed with nothing retyped, a walk-in that is taken in with what is
// known and says what is not, an estimate the customer turns down before any
// work, and more work that needs a new price agreed.

async function inAWeek(page: Page): Promise<string> {
  return page.evaluate(() => new Intl.DateTimeFormat('sv-SE', { timeZone: 'Europe/Stockholm' })
    .format(new Date(Date.now() + 7 * 86_400_000)));
}

async function book(page: Page, plate: string, customer: string, phone: string): Promise<string> {
  await page.goto('/calendar');
  const form = page.locator('form[action="/calendar"][method=post]');
  await form.locator('input[name=date]').fill(await inAWeek(page));
  await form.locator('input[name=time]').fill('10:00');
  await form.locator('input[name=minutes]').fill('60');
  await form.locator('select[name=technician_id]').selectOption({ label: tech.name });
  await form.locator('input[name=registration]').fill(plate);
  await form.locator('input[name=what]').fill('Service och bromsar');
  await form.locator('input[name=customer_name]').fill(customer);
  await form.locator('input[name=customer_phone]').fill(phone);
  await form.locator('button[type=submit]').click();
  await page.goto(`/calendar?q=${plate}`);
  await page.locator('ul.lines a[href^="/bookings/"]').first().click();
  return page.url();
}

async function invoiceAndCollect(page: Page, job: string) {
  await page.goto(job);
  await page.getByRole('button', { name: 'Invoice it' }).click();
  await page.getByRole('link', { name: /A-\d+/ }).first().click();
  const pay = page.locator('form[action$="/payments"]');
  await pay.locator('select[name=method]').selectOption('cash');
  await pay.locator('button[type=submit]').click();
  await page.goto(job);
  await page.getByRole('button', { name: 'The customer has collected it' }).click();
}

test.describe.serial('intake and the jobs that follow', () => {
  test('a booked car is taken in without retyping, and everything survives to closed', async ({ page, browser }) => {
    test.setTimeout(90_000);
    await signIn(page, desk);
    const booking = await book(page, 'INT100', 'Greta Gäst', '070-100 20 30');

    // The booking is not the car being here: no job yet.
    await expect(page.getByText('The job it became')).toHaveCount(0);

    await page.getByRole('link', { name: 'The car is here: take it in' }).click();
    // Typed once, at booking, and already in the form.
    await expect(page.locator('input[name=registration]')).toHaveValue(/INT\s?100/);
    await expect(page.locator('input[name=complaint]')).toHaveValue('Service och bromsar');
    await expect(page.getByText('Greta Gäst').first()).toBeVisible();
    // Nothing is chosen for the desk, the technician is offered and no
    // promise is made up from the slot.
    await expect(page.locator('input[name=customer][value=new]')).not.toBeChecked();
    await expect(page.locator('select[name=technician_id]')).toHaveValue(/.+/);
    await expect(page.locator('input[name=promised_at]')).toHaveValue('');

    await page.locator('input[name=customer][value=new]').check();
    await page.locator('input[name=promised_at]').fill(await shopClock(page, 60 * 30));
    await page.getByRole('button', { name: 'Open the job' }).click();
    await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
    const job = page.url();

    // The job says who, what and when -- and what it still does not know.
    await expect(page.getByText('Service och bromsar').first()).toBeVisible();
    await expect(page.getByText('Greta Gäst').first()).toBeVisible();
    await expect(page.getByText(tech.name).first()).toBeVisible();
    await expect(page.locator('#live-ready')).toContainText('Promised');
    await expect(page.locator('#next-step')).toContainText('Price the work');
    await expect(page.locator('#next-step')).toContainText('the front desk');
    await expect(page.locator('#next-step')).not.toContainText('Not known yet');
    await expect(page.locator('#next-step a[href^="/bookings/"]')).toBeVisible();
    await page.locator('#next-step a[href^="/bookings/"]').click();
    await expect(page.getByText('The job it became')).toBeVisible();

    // Priced, agreed, worked and handed back.
    await page.goto(job);
    await page.fill('input[form=add-line][name=description]', 'Bromsservice');
    await page.fill('input[form=add-line][name=quantity]', '1');
    await page.fill('input[form=add-line][name=unit_price]', '1200');
    await page.locator('button[form=add-line]').click();
    await getApproved(page);
    await expect(page.locator('#next-step')).toContainText('Start the work');
    await expect(page.locator('#next-step')).toContainText('the technician');
    await signOut(page);

    await signIn(page, tech);
    await page.goto(job);
    await expect(page.locator('#next-step')).toContainText('Start the work');
    // Not the customer, though the technician can see there is one.
    await expect(page.getByText('Greta Gäst')).toHaveCount(0);
    await expect(page.getByText('070-100 20 30')).toHaveCount(0);
    await page.getByRole('button', { name: 'Start work' }).click();
    await page.getByRole('button', { name: 'It is ready' }).click();
    await expect(page.locator('#next-step')).toContainText('Tell the customer');
    await signOut(page);

    await signIn(page, desk);
    await invoiceAndCollect(page, job);
    await expect(page.getByText('Closed').first()).toBeVisible();
    // The customer and the car are on the document they were carried to.
    await page.getByRole('link', { name: /A-\d+/ }).first().click();
    await expect(page.getByText('Greta Gäst').first()).toBeVisible();
    await expect(page.getByText(/INT\s?100/).first()).toBeVisible();
    await expect(page.getByText('Bromsservice').first()).toBeVisible();

    // And the booking, taken in once, is not offered again.
    await page.goto(booking);
    await expect(page.getByRole('link', { name: 'The car is here: take it in' })).toHaveCount(0);
    await signOut(page);
  });

  test('a walk-in is taken in with what is known and says what is not', async ({ page }) => {
    await signIn(page, desk);
    await page.goto('/jobs/new');
    await page.fill('input[name=registration]', 'INT200');
    await page.fill('[name=complaint]', 'Vibrerar i ratten');
    await page.getByRole('button', { name: 'Open the job' }).click();
    await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
    const job = page.url();

    // The car is here, and the job says what it is missing.
    await expect(page.locator('#next-step')).toContainText('Not known yet');
    for (const missing of ['no customer', 'nobody on it', 'no promised time']) {
      await expect(page.locator('#next-step')).toContainText(missing);
    }
    // Missing facts block nothing until the action that needs them.
    await expect(page.locator('#next-step')).toContainText('Price the work');
    await page.goto('/board');
    await expect(page.locator('a.card', { hasText: /INT\s?200/ })).toContainText('Next: Price the work');
    await expect(page.locator('a.card', { hasText: /INT\s?200/ })).toContainText('No customer recorded yet');

    // A second walk-in, with the customer typed at the counter.
    await page.goto('/jobs/new');
    await page.fill('input[name=registration]', 'INT201');
    await page.fill('[name=complaint]', 'Ljud från växellådan');
    await page.fill('input[name=new_name]', 'Walter Drop');
    await page.fill('input[name=new_phone]', '070-200 30 40');
    await page.locator('select[name=technician_id]').selectOption({ label: tech.name });
    await page.getByRole('button', { name: 'Open the job' }).click();
    await expect(page.getByText('Walter Drop').first()).toBeVisible();
    await expect(page.locator('#next-step')).not.toContainText('no customer');
    await expect(page.locator('#next-step')).not.toContainText('nobody on it');
    await expect(page.locator('#next-step')).toContainText('no promised time');

    // The first can still be given a customer afterwards.
    await page.goto(job);
    await page.getByRole('link', { name: 'Say who is paying' }).click();
    await page.fill('form[action$="/customer"] input[name=name]', 'Senare Kund');
    await page.getByRole('button', { name: 'Save and use' }).click();
    await expect(page.locator('#next-step')).not.toContainText('no customer');
    await signOut(page);
  });

  test('an estimate turned down before any work ends without an invoice', async ({ page }) => {
    await signIn(page, desk);
    await page.goto('/jobs/new');
    await page.fill('input[name=registration]', 'INT300');
    await page.fill('[name=complaint]', 'Kostnadsförslag på kopplingen');
    await page.fill('input[name=new_name]', 'Nej Tack');
    await page.getByRole('button', { name: 'Open the job' }).click();
    const job = page.url();
    await page.fill('input[form=add-line][name=description]', 'Byte av koppling');
    await page.fill('input[form=add-line][name=quantity]', '1');
    await page.fill('input[form=add-line][name=unit_price]', '9000');
    await page.locator('button[form=add-line]').click();
    await page.getByRole('button', { name: 'Price it' }).click();
    await page.getByRole('button', { name: 'Ask the customer' }).click();
    await expect(page.locator('#next-step')).toContainText('Put the quote to the customer');
    await page.getByRole('button', { name: 'Put the quote to the customer' }).click();
    await expect(page.locator('#next-step')).toContainText('Wait for the customer');

    await page.getByRole('button', { name: 'The customer said no' }).click();
    await expect(page.getByText('Declined').first()).toBeVisible();
    await expect(page.locator('table.lines-table')).toContainText('declined');
    // Nothing was done, so there is nothing to bill.
    await expect(page.locator('#next-step')).toContainText('Cancel the job: nothing was done');
    await page.getByRole('button', { name: 'Cancel it' }).click();
    await expect(page.getByText('Cancelled').first()).toBeVisible();
    await expect(page.locator('#next-step')).not.toContainText('Invoice');
    await expect(page.getByRole('link', { name: /A-\d+/ })).toHaveCount(0);
    await page.goto(job);
    await signOut(page);
  });

  test('more work after approval needs its own price agreed', async ({ page }) => {
    test.setTimeout(90_000);
    await signIn(page, desk);
    await page.goto('/jobs/new');
    await page.fill('input[name=registration]', 'INT400');
    await page.fill('[name=complaint]', 'Bromsservice');
    await page.fill('input[name=new_name]', 'Mer Arbete');
    await page.getByRole('button', { name: 'Open the job' }).click();
    const job = page.url();
    await page.fill('input[form=add-line][name=description]', 'Bromsskivor fram');
    await page.fill('input[form=add-line][name=quantity]', '1');
    await page.fill('input[form=add-line][name=unit_price]', '2000');
    await page.locator('button[form=add-line]').click();
    await getApproved(page);
    await page.getByRole('button', { name: 'Start work' }).click();

    // The technician finds more; the counter adds it. It is not agreed.
    await page.fill('input[form=add-line][name=description]', 'Byte av bromsslang');
    await page.fill('input[form=add-line][name=quantity]', '1');
    await page.fill('input[form=add-line][name=unit_price]', '350');
    await page.locator('button[form=add-line]').click();
    await expect(page.locator('table.lines-table tr', { hasText: 'Byte av bromsslang' })).toContainText('added after approval');

    // Its own version, with only the new line, agreed for its own amount.
    await agreeAtTheCounter(page);
    await expect(page.locator('#live-quote')).toContainText('Version 2');
    await expect(page.locator('table.lines-table tr', { hasText: 'Byte av bromsslang' })).not.toContainText('added after approval');
    await expect(page.locator('#live-quote')).toContainText('437.50');

    // Both lines are on the invoice.
    await page.getByRole('button', { name: 'It is ready' }).click();
    await invoiceAndCollect(page, job);
    await page.getByRole('link', { name: /A-\d+/ }).first().click();
    await expect(page.getByText('Bromsskivor fram').first()).toBeVisible();
    await expect(page.getByText('Byte av bromsslang').first()).toBeVisible();
    await signOut(page);
  });
});
