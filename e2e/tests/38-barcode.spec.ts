import { test, expect } from '@playwright/test';
import { owner, tech, desk, signIn, signOut, pricedJob } from './helpers';

// A part has a label, the label scans, and what a scan does is a row in the
// ledger: the bars are printed with the number in plain text under them, any
// code that belongs to the part finds it, a code nobody knows says so, and
// the technician takes the part out to a job with the same code.

test('a part is labelled, found by any of its codes, and taken out to a job by scanning', async ({ page }) => {
  test.setTimeout(150_000);
  await signIn(page, owner);
  await page.goto('/stock/parts/new');
  await page.fill('input[name=number]', 'BC-100');
  await page.fill('input[name=name]', 'Oljefilter');
  await page.fill('input[name=price]', '120');
  await page.fill('textarea[name=codes]', '7311234567890\nALT-OIL-1');
  await page.locator('form[action="/stock/parts"] button[type=submit]').click();
  await expect(page.locator('h1')).toContainText('BC-100');

  // The label: bars, and the number as text under them.
  await page.goto('/labels');
  const label = page.locator('.label', { hasText: 'BC-100' });
  await expect(label.locator('svg')).toHaveCount(1);
  await expect(label.locator('.code')).toHaveText('BC-100');

  // A scanner types the code and presses Enter, in the one field.
  await page.goto('/scan');
  await expect(page.locator('input[name=code]')).toBeFocused();
  await page.keyboard.type('ALT-OIL-1');
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(/\/scan\?code=ALT-OIL-1/);
  await expect(page.locator('.card', { hasText: 'BC-100 · Oljefilter' })).toBeVisible();

  // Book five in; the shelf says five.
  const book = page.locator('form[action="/stock/move"]');
  await book.locator('input[name=quantity]').fill('5');
  await book.locator('select[name=kind]').selectOption('received');
  await book.locator('button[type=submit]').click();
  await page.goto('/scan?code=7311234567890');
  await expect(page.locator('.card .tag').first()).toContainText('5');

  // A code nobody knows says so, and offers to add it.
  await page.goto('/scan?code=NOPE-000');
  await expect(page.getByRole('alert')).toContainText('Nothing answers to that code.');
  await expect(page.getByRole('link', { name: 'Add it to stock' })).toBeVisible();
  await signOut(page);

  // The counter takes a car in; the technician takes the part out by its code.
  await signIn(page, desk);
  const job = await pricedJob(page, 'BAR100');
  await signOut(page);
  await signIn(page, tech);
  await page.goto(job);
  await page.locator('form[action$="/takeout"] input[name=code]').fill('ALT-OIL-1');
  await page.getByRole('button', { name: 'Take it out to the job' }).click();
  await expect(page.locator('li', { hasText: 'BC-100 · Oljefilter' })).toContainText('taken out, not priced');
  await signOut(page);

  // One row in the ledger and the shelf is down by one: four.
  await signIn(page, owner);
  await page.goto('/scan?code=BC-100');
  await expect(page.locator('.card .tag').first()).toContainText('4');
  await expect(page.getByText('taken out to a job').first()).toBeVisible();

  // Retired, it is not found any more.
  await page.goto('/stock');
  await page.getByRole('link', { name: 'BC-100 · Oljefilter' }).click();
  await page.getByRole('button', { name: 'Retire it' }).click();
  await page.goto('/scan?code=ALT-OIL-1');
  await expect(page.getByRole('alert')).toContainText('Nothing answers to that code.');
});
