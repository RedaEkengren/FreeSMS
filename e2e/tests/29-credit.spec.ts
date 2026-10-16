import { test, expect } from '@playwright/test';
import { tech, desk, signIn, signOut, getApproved, nameCustomer } from './helpers';

// An issued invoice is never changed, so a correction is another document: part
// of it taken back after seeing what that leaves, the rest of it later, and when
// the wrong person was billed, all of it credited and the right one billed on a
// correction order. Every step says why and who.

test('part of an invoice credited, the rest later, and the right buyer billed', async ({ page }) => {
  test.setTimeout(150_000);
  await signIn(page, desk);
  // The person who should have been billed exists as a customer.
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', 'CRD002');
  await page.fill('[name=complaint]', 'Ekonomi');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await nameCustomer(page, 'Bolaget Ekonomi AB');

  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', 'CRD001');
  await page.fill('[name=complaint]', 'Skrapande ljud');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const job = page.url();
  await nameCustomer(page, 'Privat Person');
  for (const [text, qty, price] of [['Byte av bromsskivor', '2', '800'], ['Provkörning', '1', '300']]) {
    await page.fill('input[form=add-line][name=description]', text);
    await page.fill('input[form=add-line][name=quantity]', qty);
    await page.fill('input[form=add-line][name=unit_price]', price);
    await page.locator('button[form=add-line]').click();
    await expect(page.locator('table.lines-table tbody tr', { hasText: text })).toBeVisible();
  }
  await getApproved(page);
  await signOut(page);
  await signIn(page, tech);
  await page.goto(job);
  await page.getByRole('button', { name: 'Start work' }).click();
  await page.getByRole('button', { name: 'It is ready' }).click();
  await signOut(page);

  await signIn(page, desk);
  await page.goto(job);
  await page.getByRole('button', { name: 'Invoice it' }).click();
  await page.getByRole('link', { name: /A-\d+/ }).first().click();
  const invoice = page.url();

  // Only one brake disc was fitted: take back one of the two, after looking.
  await page.getByRole('link', { name: 'Credit part of it' }).click();
  await page.fill('input[name=qty_1]', '1');
  await page.fill('input[name=reason]', 'Bara en skiva byttes');
  await page.getByRole('button', { name: 'See what it comes to' }).click();
  await expect(page.getByRole('heading', { name: 'Check the credit' })).toBeVisible();
  await expect(page.getByText('Could be credited after')).toBeVisible();
  await expect(page.getByText('A credit is money. It does not put a part back on the shelf')).toBeVisible();
  await page.getByRole('button', { name: 'Issue the credit note' }).click();
  await expect(page.getByRole('heading', { name: /Credit note A-\d+/ })).toBeVisible();

  // Back on the original: the correction is there with its reason and who.
  await page.goto(invoice);
  await expect(page.getByRole('heading', { name: 'What has been done to correct it' })).toBeVisible();
  await expect(page.getByText('Bara en skiva byttes')).toBeVisible();

  // More than is left of a line is refused with the reason.
  await page.getByRole('link', { name: 'Credit part of it' }).click();
  await page.fill('input[name=qty_1]', '5');
  await page.fill('input[name=reason]', 'För mycket');
  await page.getByRole('button', { name: 'See what it comes to' }).click();
  await expect(page.getByText('only 1 of')).toBeVisible();

  // The wrong person was billed: the rest is credited and the right one billed.
  await page.goto(invoice);
  await page.getByRole('link', { name: 'Credit part of it' }).click();
  await page.locator('input[name=everything]').check();
  await page.fill('input[name=reason]', 'Fel mottagare');
  await page.getByRole('button', { name: 'See what it comes to' }).click();
  await expect(page.getByText('nothing left')).toBeVisible();
  await page.getByRole('button', { name: 'Issue the credit note' }).click();
  await page.goto(invoice);
  await page.getByRole('link', { name: 'The wrong person was billed' }).click();
  await page.fill('input[name=q]', 'Bolaget');
  await page.getByRole('button', { name: 'Look' }).click();
  await page.locator('form.card', { hasText: 'Bolaget Ekonomi AB' }).locator('input[name=reason]').fill('Bolaget är betalare');
  await page.locator('form.card', { hasText: 'Bolaget Ekonomi AB' }).getByRole('button', { name: 'Bill them instead' }).click();
  await expect(page.getByRole('heading', { name: /Invoice A-\d+/ })).toBeVisible();
  await expect(page.locator('article').getByText('Bolaget Ekonomi AB')).toBeVisible();
  await expect(page.getByText('Replaces A-')).toBeVisible();
  await signOut(page);
});
