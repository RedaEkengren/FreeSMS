import { test, expect } from '@playwright/test';
import { owner, desk, tech, signIn, signOut } from './helpers';

// Getting ready for the first customer: the owner sees what is missing and why,
// puts it right through the forms the list links to, says what cannot be read
// from a setting, looks at an invoice that is only an example, and ends with
// the list saying the workshop is ready. The counter and the shop floor do not
// get the list, and everybody gets help written for what they do.

test('the owner works through the list until the workshop is ready', async ({ page }) => {
  test.setTimeout(150_000);
  await signIn(page, desk);
  expect((await page.goto('/start'))?.status()).toBe(403);
  // Help is for everyone, in the words of their work.
  await page.goto('/help');
  await expect(page.getByRole('heading', { name: 'At the counter' })).toBeVisible();
  await expect(page.getByText('Choose Take in a vehicle')).toBeVisible();
  expect((await page.goto('/help/owner'))?.status()).toBe(403);
  await signOut(page);

  await signIn(page, tech);
  await page.goto('/help');
  await expect(page.getByRole('heading', { name: 'In the workshop' })).toBeVisible();
  await signOut(page);

  await signIn(page, owner);
  await page.getByRole('link', { name: 'Getting started' }).click();
  await expect(page.getByRole('heading', { name: 'Getting started' })).toBeVisible();
  await expect(page.getByText(/needed things are done/)).toBeVisible();
  // It says what is wrong with the one required thing, and links to the form.
  const decisions = page.locator('li#backups, li#support, li#accountant');
  await expect(decisions.first()).toBeVisible();

  // Say how the workshop works: a one-person shop is not asked for staff.
  await page.locator('form[action="/start/step/operating_model"] input[value=solo]').check();
  await page.locator('form[action="/start/step/operating_model"]').getByRole('button', { name: 'Save' }).click();
  await expect(page.locator('form[action="/start/step/operating_model"] input[value=solo]')).toBeChecked();
  await expect(page.locator('li#staff')).toContainText('technicians');

  // The company details, through the form the list links to.
  await page.locator('li#seller').getByRole('link', { name: 'Open' }).click();
  await expect(page).toHaveURL(/\/shop$/);
  await page.fill('input[name=address_line1]', 'Verkstadsgatan 1');
  await page.fill('input[name=postal_code]', '411 38');
  await page.fill('input[name=city]', 'Göteborg');
  await page.fill('input[name=org_number]', '556677-8899');
  await page.fill('input[name=vat_number]', 'SE556677889901');
  await page.fill('input[name=payment_reference]', 'Bankgiro 123-4567');
  await page.locator('form[action="/shop"]').getByRole('button', { name: 'Save' }).click();
  await page.goto('/start');
  for (const key of ['seller', 'payment', 'vat']) {
    await expect(page.locator('li#' + key)).toContainText('done');
  }

  // A person is named, not just ticked.
  const backups = page.locator('li#backups form');
  await backups.locator('input[name=note]').fill('Olle Ägare, 070-111 22 33');
  await backups.getByRole('button', { name: 'This person owns the backups' }).click();
  await expect(page.locator('li#backups')).toContainText('Olle Ägare, 070-111 22 33');

  // The invoice to look at is an example: no invoice, no number.
  await page.locator('li#invoice').getByRole('link', { name: 'Open' }).click();
  await expect(page.getByText('No invoice has been made and no number has been used.')).toBeVisible();
  await expect(page.getByText('Verkstadsgatan 1')).toBeVisible();
  await expect(page.getByText('556677-8899')).toBeVisible();
  await expect(page.getByText('Complete', { exact: true })).toBeVisible();
  await page.getByRole('button', { name: 'The invoice looks right' }).click();
  await expect(page.locator('li#invoice')).toContainText('done');

  // The rest: said, not guessed.
  await page.locator('li#locale form').getByRole('button', { name: 'These are right' }).click();
  // Earlier tests in this workshop may already have imported or chosen a
  // final check, which is the other way of having decided.
  for (const [key, label] of [['data', 'Start clean'], ['final_check', 'We do not use a final check']]) {
    const b = page.locator(`li#${key} form`).getByRole('button', { name: label });
    if (await b.count()) await b.click();
    else await expect(page.locator('li#' + key)).toContainText('done');
  }
  for (const [key, label, who] of [['accountant', 'My accountant has read an export', 'Rut på Redovisning, 031-555 01 02'], ['support', 'This is who we ring', 'Hosten, 08-123 45']]) {
    const f = page.locator(`li#${key} form`);
    await f.locator('input[name=note]').fill(who);
    await f.getByRole('button', { name: label }).click();
  }
  await expect(page.getByText('Ready for the first customer')).toBeVisible();
  // A provider that is set up is said; one that is not is a manual way, not a
  // missing step.
  await expect(page.locator('li#messaging')).toContainText('email is set up');
  await expect(page.locator('li#lookup')).toContainText('by hand');

  // And nothing was stopped on the way: a job can be taken in.
  await page.goto('/jobs/new');
  await expect(page.locator('input[name=registration]')).toBeVisible();
  await signOut(page);
});
