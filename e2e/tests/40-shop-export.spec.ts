import { test, expect } from '@playwright/test';
import { owner, desk, signIn, signOut } from './helpers';

// The owner takes the whole workshop with them: a file built in the
// background, listed with who took it, a zip with a CSV per table, a manifest
// and a README, and nothing a new system could use to sign in. Nobody else is
// given the button, or the file.

test('the owner takes the whole workshop out as one file, and nobody else can', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page, desk);
  await page.goto('/privacy');
  await expect(page.getByRole('button', { name: 'Export the whole workshop' })).toHaveCount(0);
  expect((await page.request.post('/privacy/export', { headers: { Origin: new URL(page.url()).origin } })).status()).toBe(403);
  await signOut(page);

  await signIn(page, owner);
  await page.goto('/privacy');
  await page.getByRole('button', { name: 'Export the whole workshop' }).click();
  await expect(page).toHaveURL(/\/privacy$/);

  // Built after the request returns; the list says so until it is done.
  const row = page.locator('ul.lines li', { hasText: owner.name }).first();
  await expect(row).toBeVisible();
  let link = row.getByRole('link', { name: 'Download' });
  for (let i = 0; i < 40 && (await link.count()) === 0; i++) {
    await page.waitForTimeout(500);
    await page.reload();
    link = page.locator('ul.lines li', { hasText: owner.name }).first().getByRole('link', { name: 'Download' });
  }
  await expect(link).toBeVisible();
  const href = (await link.getAttribute('href'))!;

  const file = await page.request.get(href);
  expect(file.status()).toBe(200);
  const zip = await file.body();
  // A zip, whose file names are stored in the clear.
  expect(zip.subarray(0, 2).toString()).toBe('PK');
  const text = zip.toString('latin1');
  for (const name of ['manifest.json', 'README.txt', 'customers.csv', 'vehicles.csv', 'invoices.csv', 'invoice_lines.csv', 'stock_movements.csv']) {
    expect(text).toContain(name);
  }
  // Nothing to sign in with goes along.
  for (const name of ['sessions.csv', 'login_attempts.csv']) {
    expect(text).not.toContain(name);
  }
  await signOut(page);

  // The file is the owner's alone.
  await signIn(page, desk);
  expect((await page.request.get(href)).status()).toBe(403);
});
