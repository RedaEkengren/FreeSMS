import { test, expect } from '@playwright/test';
import { owner, desk, signIn, signOut } from './helpers';

// Starting from a file: a Swedish export is read and previewed before anything
// is written, a row that may be somebody already here needs an answer, the
// same file twice is the same upload, and opening stock lands in the ledger
// as a dated entry.

async function upload(page: any, kind: string, name: string, text: string) {
  await page.goto('/import');
  await page.locator('select[name=kind]').selectOption(kind);
  await page.locator('input[name=file]').setInputFiles({ name, mimeType: 'text/csv', buffer: Buffer.from(text, 'utf-8') });
  await page.getByRole('button', { name: 'Read the file' }).click();
}

test('a customer list, parts and opening stock imported from Swedish files', async ({ page }) => {
  test.setTimeout(150_000);
  // Only the owner starts an import.
  await signIn(page, desk);
  const refused = await page.goto('/import');
  expect(refused?.status()).toBe(403);
  await signOut(page);

  await signIn(page, owner);
  const customers =
    'kundnr;typ;namn;telefon;epost;adress;postnr;ort\n' +
    'K-1;privat;Åsa Öberg;070-123 45 67;asa@example.se;Storgatan 1;411 38;Göteborg\n' +
    'K-2;företag;Bilfirman AB;031-12 34 56;;Hamnvägen 4;418 34;Göteborg\n' +
    'K-3;privat;Åsa Öberg igen;0701234567;;;;\n' +
    'K-4;privat;;;;;;\n';
  await upload(page, 'customers', 'kunder.csv', customers);
  await expect(page.getByRole('heading', { name: 'kunder.csv' })).toBeVisible();
  // Nothing is written yet; the preview says what each row would do.
  await expect(page.getByText('Åsa Öberg').first()).toBeVisible();
  await expect(page.getByText('would be created').first()).toBeVisible();
  await expect(page.getByText('name is missing')).toBeVisible();
  await expect(page.getByText('This may be somebody who is already here').first()).toBeVisible();

  // The row that shares a telephone number needs an answer before it goes.
  await page.getByRole('button', { name: 'Apply' }).click();
  await expect(page.getByRole('heading', { name: 'kunder.csv' })).toBeVisible();
  await page.locator('input[name=d_2][value=create]').check();
  await page.locator('input[name=d_4][value=skip]').check();
  await page.getByLabel('Import the others').check();
  await page.getByRole('button', { name: 'Apply' }).click();
  await expect(page.getByText(/Applied .* by /)).toBeVisible();
  await expect(page.getByText('What the file said has been removed')).toBeVisible();
  await expect(page.getByText('asa@example.se')).toHaveCount(0);

  // The same file is the same upload: nothing is made twice.
  await upload(page, 'customers', 'kopia.csv', customers);
  await expect(page.getByRole('heading', { name: 'kunder.csv' })).toBeVisible();
  await page.goto('/customers');
  await expect(page.getByText('Åsa Öberg igen')).toHaveCount(0);
  await expect(page.getByText('Bilfirman AB')).toHaveCount(1);

  // Parts, and then the shelf on a day.
  await upload(page, 'parts', 'delar.csv', 'artikelnummer;benämning;enhet;inpris;utpris\nIMP-100;Bromsklossar fram;st;450,00;650,00\nIMP-OIL;Motorolja 5W-30;liter;89,50;129,00\n');
  await page.getByRole('button', { name: 'Apply' }).click();
  await expect(page.getByText(/Applied .* by /)).toBeVisible();
  await upload(page, 'stock', 'lager.csv', 'part_number;quantity;as_of;unit_cost\nIMP-100;6;2026-01-31;450,00\nIMP-OIL;48,5;2026-01-31;\nIMP-NOPE;1;2026-01-31;\n');
  await expect(page.getByText('import the parts first')).toBeVisible();
  await page.getByLabel('Import the others').check();
  await page.getByRole('button', { name: 'Apply' }).click();
  await expect(page.getByText(/Applied .* by /)).toBeVisible();
  await page.goto('/stock');
  await expect(page.getByText('Motorolja 5W-30')).toBeVisible();
  await expect(page.getByText('48.5').first()).toBeVisible();

  // A different file for the same part and day does not stack on top.
  await upload(page, 'stock', 'lager2.csv', 'part_number;quantity;as_of\nIMP-100;6;2026-01-31\nIMP-OIL;50;2026-01-31\n');
  await expect(page.getByText('already recorded with another quantity').first()).toBeVisible();
  await expect(page.getByText('this opening balance is already recorded')).toBeVisible();
  await signOut(page);
});
