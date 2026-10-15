import { test, expect, Page } from '@playwright/test';
import { desk, signIn, signOut } from './helpers';

// A customer asks what something costs before there is a car in the shop.
// The desk puts a price to them with its terms, the customer reads every
// amount on their own link and says yes to part of it, the accepted work is
// booked and then taken in -- and none of it is typed twice, nor priced again.

async function addLine(page: Page, kind: string, what: string, qty: string, price: string) {
  const form = page.locator('form[action$="/lines"]');
  await form.locator('select[name=kind]').selectOption(kind);
  await form.locator('input[name=description]').fill(what);
  await form.locator('input[name=quantity]').fill(qty);
  await form.locator('input[name=unit_price]').fill(price);
  await form.locator('button[type=submit]').click();
  await expect(page.getByText(what).first()).toBeVisible();
}

test('an enquiry becomes a booking and then a job at the price the customer was shown', async ({ page, browser }) => {
  test.setTimeout(90_000);
  await signIn(page, desk);
  await page.goto('/board');
  const carsHere = await page.locator('a.card').count();
  await page.goto('/enquiries');
  // Nothing but what they asked about: no registration, no name.
  await page.fill('input[name=subject]', 'Bromsar fram och byte av skivor');
  await page.getByRole('button', { name: 'Start an enquiry' }).click();
  await expect(page).toHaveURL(/\/enquiries\/[0-9a-f-]{36}$/);
  const enquiry = page.url();
  await expect(page.locator('dl')).toContainText('not known yet');
  await expect(page.getByText('Nothing here means the car has arrived')).toBeVisible();

  await addLine(page, 'labour', 'Byte av bromsskivor', '2', '800');
  await addLine(page, 'part', 'Bromsskivor, par', '1', '900');
  await page.fill('input[name=valid_days]', '10');
  await page.selectOption('select[name=price_kind]', 'estimate');
  await page.fill('textarea[name=limits]', 'Felsökning upp till en timme ingår');
  await page.getByRole('button', { name: 'Put it to the customer' }).click();
  await expect(page.getByText('Version 1').first()).toBeVisible();
  await expect(page.locator('.card').first()).toContainText('An estimate');
  await expect(page.locator('.card').first()).toContainText('3,125.00');

  // The printable proposal.
  await page.getByRole('link', { name: 'Print this version' }).click();
  await expect(page.getByText('Felsökning upp till en timme ingår')).toBeVisible();
  await expect(page.getByText('3,125.00')).toBeVisible();
  await page.goBack();

  // Nothing is a car yet.
  await page.goto('/board');
  await expect(page.locator('a.card')).toHaveCount(carsHere);
  await page.goto(enquiry);

  // The customer's own page.
  await page.getByRole('button', { name: 'Make a link for the customer' }).click();
  const link = new URL(await page.locator('#share-url').inputValue());
  const phoneCtx = await browser.newContext();
  const phone = await phoneCtx.newPage();
  try {
    const url = new URL(link.pathname, page.url()).toString();
    await phone.goto(url);
    await expect(phone.getByText('Bromsar fram och byte av skivor')).toBeVisible();
    await expect(phone.getByText('1,600.00')).toBeVisible();
    await expect(phone.getByText('3,125.00')).toBeVisible();
    await expect(phone.getByText('Felsökning upp till en timme ingår')).toBeVisible();

    // The desk replaces the version while the customer reads: the old form
    // answers nothing and sends them to the new one.
    await page.fill('textarea[name=limits]', 'Felsökning upp till en timme ingår');
    await page.fill('input[name=valid_days]', '10');
    await page.selectOption('select[name=price_kind]', 'fixed');
    await addLine(page, 'fee', 'Miljöavgift', '1', '100');
    await page.getByRole('button', { name: 'Put a new version to the customer' }).click();
    await expect(page.getByText('Version 2').first()).toBeVisible();
    await phone.getByRole('button', { name: 'Yes, do it' }).click();
    await expect(phone.getByText('changed the proposal while you were reading it')).toBeVisible();
    await expect(phone.locator('td', { hasText: 'Miljöavgift' })).toBeVisible();
    await page.reload();
    await expect(page.getByText('Book what they accepted')).toHaveCount(0);

    // A yes to part of it: the discs are bought elsewhere.
    await phone.getByLabel('Bromsskivor, par').uncheck();
    await phone.getByRole('button', { name: 'Yes, do it' }).click();
    await expect(phone.getByText('Not all the lines.')).toBeVisible();
  } finally {
    await phoneCtx.close();
  }

  await page.goto(enquiry);
  await expect(page.locator('h1 + .tag, .row .tag').first()).toContainText('Partly accepted');
  await page.locator('form[action$="/book"] input[name=date]').fill('2027-03-02');
  await page.getByRole('button', { name: 'Book it' }).click();
  await expect(page).toHaveURL(/\/bookings\/[0-9a-f-]{36}$/);
  await expect(page.getByText('Bromsar fram och byte av skivor').first()).toBeVisible();
  // Booked is not here.
  await page.goto('/board');
  await expect(page.locator('a.card')).toHaveCount(carsHere);

  // The car arrives: the form is filled from the booking, and the job has
  // the lines that were accepted, at the price shown.
  await page.goto('/calendar?q=Bromsar');
  await page.locator('ul.lines a[href^="/bookings/"]').first().click();
  await page.getByRole('link', { name: 'The car is here: take it in' }).click();
  await expect(page.locator('input[name=complaint]')).toHaveValue('Bromsar fram och byte av skivor');
  await page.fill('input[name=registration]', 'ENQ123');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const lines = page.locator('table.lines-table');
  await expect(lines).toContainText('Byte av bromsskivor');
  await expect(lines).toContainText('Miljöavgift');
  await expect(lines).not.toContainText('Bromsskivor, par');
  await expect(lines).toContainText('1,600.00');
  await expect(page.getByRole('link', { name: 'The enquiry and the price they accepted' })).toBeVisible();
  // Agreed on the enquiry: no second round of asking to go ahead.
  await expect(page.getByRole('button', { name: 'Put the quote to the customer' })).toHaveCount(0);
  await page.getByRole('button', { name: 'Price it' }).click();
  await page.getByRole('button', { name: 'Ask the customer' }).click();
  await page.getByRole('button', { name: 'Mark it approved' }).click();
  await expect(page.getByText('Approved, not started').first()).toBeVisible();
  await signOut(page);
});
