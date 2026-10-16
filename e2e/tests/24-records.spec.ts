import { test, expect, Page } from '@playwright/test';
import { desk, signIn, signOut } from './helpers';

// Keeping the customer and vehicle records straight between visits: a changed
// number is corrected with who and when left on the page, a stale form is told
// somebody got there first, a plate typed wrong is rewritten while a real new
// plate is added, a second copy of somebody is asked about before it is made,
// and two records that are one are joined after a page that says what moves.

async function takeIn(page: Page, registration: string): Promise<string> {
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', registration);
  await page.fill('[name=complaint]', `Register ${registration}`);
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  return page.url();
}

async function newCustomer(page: Page, job: string, name: string, phone: string, confirm = false) {
  await page.goto(`${job}/customer`);
  await page.locator('form[action$="/customer"] input[name=name]').fill(name);
  await page.locator('form[action$="/customer"] input[name=phone]').fill(phone);
  await page.getByRole('button', { name: 'Save and use' }).click();
  if (confirm) {
    await expect(page.getByRole('heading', { name: 'Is this somebody we already have?' })).toBeVisible();
    await page.getByRole('button', { name: 'They are different: make a new customer' }).click();
  }
}

test('a changed number, a mistaken plate, a sold car and two records that are one', async ({ page, browser }) => {
  test.setTimeout(120_000);
  await signIn(page, desk);
  const first = await takeIn(page, 'REC001');
  await newCustomer(page, first, 'Greta Gren', '073-555 01 23');

  // The register finds her by the end of her number, and the card lists the car.
  await page.goto('/customers');
  await page.fill('input[name=q]', '5550123');
  await page.getByRole('button', { name: 'Look' }).click();
  await page.getByRole('link', { name: /Greta Gren/ }).click();
  await expect(page.getByRole('link', { name: /REC001/ }).first()).toBeVisible();
  const card = page.url();

  // A changed number is corrected with a reason; the old one stays in the log.
  await page.fill('input[name=phone]', '073-555 99 88');
  await page.fill('input[name=reason]', 'Rang to say the number changed');
  await page.getByRole('button', { name: 'Save the changes' }).click();
  await expect(page.getByText('saved')).toBeVisible();
  await expect(page.locator('ul.lines', { hasText: 'Rang to say the number changed' })).toContainText('073-555 01 23');

  // A second person has the card open from before and is too late.
  const other = await browser.newContext();
  const second = await other.newPage();
  await signIn(second, desk);
  await second.goto(card);
  await page.fill('input[name=phone]', '073-555 11 11');
  await page.getByRole('button', { name: 'Save the changes' }).click();
  await second.fill('input[name=phone]', '073-555 22 22');
  await second.getByRole('button', { name: 'Save the changes' }).click();
  await expect(second.getByText('Somebody else got there first')).toBeVisible();
  await other.close();

  // The plate was typed wrong, and the car then really changes plate.
  await page.getByRole('link', { name: /REC001/ }).first().click();
  await page.fill('form:has(input[value=correct]) input[name=registration]', 'REC002');
  await page.getByRole('button', { name: 'Correct the plate' }).click();
  await expect(page.locator('.plate').first()).toContainText('REC002');
  await expect(page.getByText('Plates it has carried')).toHaveCount(0);
  await page.fill('form:has(input[value=change]) input[name=registration]', 'PRIV 1');
  await page.getByRole('button', { name: 'It changed plate' }).click();
  await expect(page.getByText('Plates it has carried')).toBeVisible();

  // The car is sold: the old owner's job stays with them.
  await page.getByRole('link', { name: 'The vehicle was sold' }).click();
  await page.fill('input[name=q]', 'Greta');
  await page.getByRole('button', { name: 'Look' }).click();
  await expect(page.getByRole('button', { name: 'They are the new owner' }).first()).toBeVisible();
  await page.getByRole('button', { name: 'End their ownership' }).click();
  await expect(page.locator('dl')).not.toContainText('Greta Gren');

  // The same number typed for a second customer is asked about first.
  const next = await takeIn(page, 'REC003');
  await newCustomer(page, next, 'G. Gren', '073-555 11 11', true);
  await page.goto('/customers');
  await expect(page.getByText('G. Gren')).toBeVisible();

  // Two records that are one: the review says what moves, then they are joined.
  await page.getByRole('link', { name: /G\. Gren/ }).click();
  await page.getByRole('link', { name: 'Is this the same as another record?' }).click();
  await expect(page.getByText('the same telephone number')).toBeVisible();
  await page.getByRole('link', { name: /Greta Gren/ }).click();
  await expect(page.getByText('moves to the kept customer')).toBeVisible();
  await page.getByRole('button', { name: 'Yes, they are the same: merge' }).click();
  await expect(page.getByText('Records merged into this one')).toBeVisible();
  await signOut(page);
});
