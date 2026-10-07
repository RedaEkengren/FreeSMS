import { test, expect } from '@playwright/test';
import { owner, tech, desk, signIn, signOut, leave, readIn, pricedJob } from './helpers';

// The marketing page's customer's-link scene, performed against the real
// product in Swedish: a technician fails an item with a note and a
// photograph, the front desk makes a link, the customer -- no account, a
// browser of their own -- sees the photograph and says yes, and the
// workshop reads the answer back.
//
// Then the front desk sees the yes on the board and the job, and makes it a
// line in one step (#87).
const pixel = Buffer.from(
  '/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/wAALCAABAAEBAREA/8QAFAABAAAAAAAAAAAAAAAAAAAACf/EABQQAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQEAAD8AVN//2Q==',
  'base64');

// The customer's page speaks the workshop's language: a customer has no
// account and no setting. The suite's workshop is English, so it is a
// Swedish one for the length of this test.
async function workshopIn(page: import('@playwright/test').Page, locale: string) {
  await signIn(page, owner);
  await page.goto('/shop');
  const details = page.locator('form[action="/shop"]');
  await details.locator('select[name=locale]').selectOption(locale);
  await details.locator('button[type=submit]').click();
  await leave(page);
}

test.describe.serial('the customer link scene', () => {
  test('a photographed finding goes to the customer, and their yes becomes a line on the job', async ({ page, browser }) => {
    // The car, taken in while the workshop is still the suite's.
    await signIn(page, desk);
    const job = await pricedJob(page, 'CUS123');
    await signOut(page);

    await workshopIn(page, 'sv');
    try {

    await signIn(page, tech);
    await readIn(page, 'sv');
    let inspection = '';
    try {
      await page.goto(job);
      await page.getByRole('button', { name: 'Starta', exact: true }).click();
      await expect(page).toHaveURL(/\/inspections\//);
      inspection = page.url();
      const item = page.locator('.card', { hasText: 'Bromsar fram' });
      await item.locator('input[name=note]').fill('2 mm kvar, ojämnt slitna');
      await item.getByRole('button', { name: 'Underkänd' }).click();
      await expect(page.locator('.card', { hasText: 'Bromsar fram' }).locator('.tag.late')).toHaveText('underkänd');
      const photo = page.locator('.card', { hasText: 'Bromsar fram' }).locator('form[action$="/photo"]');
      await photo.locator('input[type=file]').setInputFiles({ name: 'pads.jpg', mimeType: 'image/jpeg', buffer: pixel });
      await photo.getByRole('button', { name: 'Lägg till fotot' }).click();
      await expect(page.locator('.card', { hasText: 'Bromsar fram' }).locator('.photos img')).toBeVisible();
      await page.getByRole('button', { name: 'Avsluta kontrollen' }).click();
    } finally {
      await readIn(page, '');
      await leave(page);
    }

    // The front desk makes the link, shown once.
    await signIn(page, desk);
    await readIn(page, 'sv');
    try {
      await page.goto(inspection);
      await page.getByRole('button', { name: 'Gör en länk till kunden' }).click();
      await expect(page.getByText('Skicka det här till kunden.')).toBeVisible();
      const link = new URL(await page.locator('#share-url').inputValue());

      // The customer: another browser, nobody signed in.
      const phone = await browser.newContext({ baseURL: page.url() });
      const customer = await phone.newPage();
      await customer.goto(link.pathname);
      const card = customer.locator('.card', { hasText: 'Bromsar fram' });
      await expect(card.getByText('behöver göras')).toBeVisible();
      await expect(card.getByText('2 mm kvar, ojämnt slitna')).toBeVisible();
      await expect(card.locator('.photos img')).toBeVisible();
      await expect(customer.getByText(/visar inga priser/)).toBeVisible();
      await card.getByRole('button', { name: 'Ja, gör det' }).click();
      await expect(customer.getByText('Du sa ja till det här. Du kan ändra dig.')).toBeVisible();
      await phone.close();

      // Back at the workshop, the answer is on the check...
      await page.goto(inspection);
      await expect(page.locator('.card', { hasText: 'Bromsar fram' }).getByText('Kunden godkände det här')).toBeVisible();

      // ...and on the board and the job, where it is priced in one step.
      await page.goto('/board');
      await expect(page.locator('a.card', { hasText: /CUS\s?123/ })).toContainText('1 sak godkänd av kunden, inte prissatt');
      await page.goto(job);
      const answer = page.locator('li', { hasText: 'Bromsar fram: 2 mm kvar, ojämnt slitna' }).filter({ has: page.locator('form') });
      await expect(answer.getByText('godkänd, inte prissatt')).toBeVisible();
      await expect(answer.getByText('via länken')).toBeVisible();
      await answer.locator('input[name=unit_price]').fill('1290');
      await answer.getByRole('button', { name: 'Gör det till en rad' }).click();
      await expect(page.locator('table.lines-table')).toContainText('Bromsar fram: 2 mm kvar, ojämnt slitna');
      await expect(page.locator('table.lines-table')).toContainText('1 290,00 kr');
      await expect(page.locator('li', { hasText: 'Bromsar fram: 2 mm kvar, ojämnt slitna' }).getByText('på jobbet', { exact: true })).toBeVisible();
      await page.goto('/board');
      await expect(page.locator('a.card', { hasText: /CUS\s?123/ })).not.toContainText('godkänd av kunden');
    } finally {
      await readIn(page, '');
      await leave(page);
    }
    } finally {
      await workshopIn(page, 'en');
    }
  });
});
