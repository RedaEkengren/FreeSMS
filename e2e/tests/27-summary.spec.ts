import { test, expect } from '@playwright/test';
import { desk, tech, signIn, signOut, getApproved } from './helpers';

// What the customer is given back with the car: built from what was recorded
// as done, handed to whoever collects it, frozen, and corrected by a new
// version. The technician suggests something for later; the counter turns it
// into an enquiry nobody has been told about.

test('a repair summary handed to a brother, then corrected', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page, desk);
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', 'SUM001');
  await page.fill('[name=complaint]', 'Skrapande ljud fram');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const job = page.url();
  await page.fill('input[form=add-line][name=description]', 'Byte av bromsskivor fram');
  await page.fill('input[form=add-line][name=quantity]', '2');
  await page.fill('input[form=add-line][name=unit_price]', '800');
  await page.locator('button[form=add-line]').click();
  await expect(page.locator('table.lines-table tbody tr', { hasText: 'Byte av bromsskivor' })).toBeVisible();
  await page.fill('input[form=add-line][name=description]', 'Byte av avgasrör');
  await page.fill('input[form=add-line][name=quantity]', '1');
  await page.fill('input[form=add-line][name=unit_price]', '900');
  await page.locator('button[form=add-line]').click();
  await expect(page.locator('table.lines-table tbody tr', { hasText: 'avgasrör' })).toBeVisible();
  await getApproved(page);
  await page.getByRole('button', { name: 'Start work' }).click();
  // Only the brakes are done.
  await page.locator('table.lines-table tbody tr', { hasText: 'bromsskivor' }).getByRole('button', { name: 'Mark done' }).click();
  await signOut(page);

  // The technician suggests something for later, and cannot hand the car over.
  await signIn(page, tech);
  await page.goto(job);
  await page.fill('form[action$="/recommendations"] input[name=body]', 'Byt bakre bromsskivor');
  await page.fill('form[action$="/recommendations"] input[name=when]', 'vid nästa service');
  await page.getByRole('button', { name: 'Suggest it' }).click();
  await page.goto(`${job}/summary`);
  await expect(page.getByText('Not for your role')).toBeVisible();
  await signOut(page);

  // The counter hands it over to the customer's brother; the exhaust is not done.
  await signIn(page, desk);
  await page.goto(`${job}/summary`);
  await expect(page.getByText('Byt bakre bromsskivor')).toBeVisible();
  await page.fill('input[name=delivered_to]', 'Karl, hennes bror');
  await page.fill('textarea[name=instructions]', 'Kör in de nya skivorna försiktigt de första 200 km.');
  await page.getByRole('button', { name: 'Hand it over' }).click();
  await expect(page.getByRole('heading', { name: 'Repair summary' })).toBeVisible();
  await expect(page.getByText('Version 1')).toBeVisible();
  await expect(page.getByText('Karl, hennes bror')).toBeVisible();
  await expect(page.getByRole('heading', { name: 'What was done' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Agreed, not done yet' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Worth doing later' })).toBeVisible();
  await expect(page.getByText('not a statement that the car is safe')).toBeVisible();
  await expect(page.locator('main')).not.toContainText('800');

  // The exhaust is done afterwards; the corrected version replaces the first.
  await page.goto(job);
  await page.locator('table.lines-table tbody tr', { hasText: 'avgasrör' }).getByRole('button', { name: 'Mark done' }).click();
  await page.goto(`${job}/summary`);
  await page.fill('input[name=delivered_to]', 'Greta Gren');
  await page.getByRole('button', { name: 'Hand over a corrected version' }).click();
  await expect(page.getByText('Version 2')).toBeVisible();
  await expect(page.getByText('replaces version 1')).toBeVisible();
  await page.goto(`${job}/summary/1`);
  await expect(page.getByText('a later version has replaced this one')).toBeVisible();
  await signOut(page);
});
