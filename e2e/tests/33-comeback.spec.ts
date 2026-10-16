import { test, expect } from '@playwright/test';
import { tech, desk, signIn, signOut, getApproved } from './helpers';

// A car that comes back: the counter opens a return visit from the finished
// job, which decides nothing about whose fault it was; a position needs its
// reason; a supplier claim needs the case to be called warranty and can be
// rejected and argued; the technician sees what was reported and no money.

test('a car comes back, the shop takes a position, and a supplier claim is argued', async ({ page }) => {
  test.setTimeout(150_000);
  await signIn(page, desk);
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', 'TIL123');
  await page.fill('[name=complaint]', 'Bromsljud');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const job = page.url();
  await page.getByRole('link', { name: 'Say who is paying' }).click();
  await page.fill('form[action$="/customer"] input[name=name]', 'Tilda Tillbaka');
  await page.getByRole('button', { name: 'Save and use' }).click();
  await expect(page).toHaveURL(job);
  await page.fill('input[form=add-line][name=description]', 'Byt bromsklossar fram');
  await page.fill('input[form=add-line][name=unit_price]', '1800');
  await page.locator('button[form=add-line]').click();
  await expect(page.locator('table.lines-table tbody tr', { hasText: 'Byt bromsklossar fram' })).toBeVisible();
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

  // Two weeks later: it squeals again.
  await page.goto(job);
  await page.getByRole('link', { name: 'Return visit' }).click();
  await page.fill('input[name=symptoms]', 'Gnisslar igen vid låg fart');
  await page.fill('input[name=odometer]', '81500');
  await page.getByRole('button', { name: 'Open a return visit' }).click();
  await expect(page.getByRole('heading', { name: 'What was reported' })).toBeVisible();
  await expect(page.getByText('Gnisslar igen vid låg fart').first()).toBeVisible();
  await expect(page.getByText('Looking into it decides nothing.')).toBeVisible();
  // Nothing is decided by opening it, and a claim is not offered yet.
  await expect(page.getByText('A claim can be filed once the case is called warranty.')).toBeVisible();
  const follow = page.locator('a', { hasText: 'The follow-up job' });
  const followUrl = new URL((await follow.getAttribute('href'))!, page.url()).toString();

  // A verdict needs its reason.
  const position = page.locator('form:has(input[value=classify])');
  await position.locator('select[name=kind]').selectOption('warranty');
  await position.getByRole('button', { name: 'Save' }).click();
  await expect(page.getByText('say why')).toBeVisible();
  await page.goBack();
  await position.locator('select[name=kind]').selectOption('warranty');
  await position.locator('input[name=note]').fill('Klossen släppte från underlaget');
  await position.getByRole('button', { name: 'Save' }).click();
  await expect(page.getByText('Klossen släppte från underlaget').first()).toBeVisible();

  // The supplier is asked, says no, and is argued with.
  const claim = page.locator('form:has(input[value=claim])');
  await claim.locator('input[name=supplier]').fill('Bromsgrossen');
  await claim.locator('input[name=amount]').fill('900');
  await claim.getByRole('button', { name: 'File a claim' }).click();
  const answer = page.locator('form:has(input[value=answer])');
  await answer.locator('select[name=status]').selectOption('rejected');
  await answer.locator('input[name=note]').fill('Slitage, säger de');
  await answer.getByRole('button', { name: 'Record their answer' }).click();
  await expect(page.getByText('claim rejected').first()).toBeVisible();
  await page.locator('form:has(input[value=answer])').locator('select[name=status]').selectOption('accepted');
  await page.locator('form:has(input[value=answer])').locator('input[name=amount]').fill('600');
  await page.locator('form:has(input[value=answer])').getByRole('button', { name: 'Record their answer' }).click();
  await expect(page.getByText('claim accepted').first()).toBeVisible();
  await signOut(page);

  // The technician sees what was reported on the follow-up job, and no case.
  await signIn(page, tech);
  await page.goto(followUrl);
  await expect(page.getByText('Gnisslar igen vid låg fart').first()).toBeVisible();
  await expect(page.getByText('Return visit').first()).toBeVisible();
  await expect(page.getByRole('link', { name: 'Return visit' })).toHaveCount(0);
  const res = await page.goto(followUrl + '/comeback');
  expect(res?.status()).toBe(403);
  await signOut(page);
});
