import { test, expect } from '@playwright/test';
import { tech, desk, owner, signIn, signOut, getApproved } from './helpers';

// Closing the day: what was invoiced and credited kept apart from what was
// recorded as arrived and paid out, every figure a link to its source, a
// comparison with the bank that needs a reason when it differs, the owner's
// sign-off, and the day saying so when it changes after it was signed.

test('a day compared with the bank, signed off, and changed afterwards', async ({ page }) => {
  test.setTimeout(150_000);
  await signIn(page, desk);
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', 'DAG123');
  await page.fill('[name=complaint]', 'Service');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const job = page.url();
  await page.getByRole('link', { name: 'Say who is paying' }).click();
  await page.fill('form[action$="/customer"] input[name=name]', 'Dagny Dagskassa');
  await page.getByRole('button', { name: 'Save and use' }).click();
  await expect(page).toHaveURL(job);
  for (const [text, price] of [['Service', '1600'], ['Torkarblad', '400']]) {
    await page.fill('input[form=add-line][name=description]', text);
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
  const pay = page.locator('form[action$="/payments"]');
  await pay.locator('input[name=amount]').fill('1000');
  await pay.locator('select[name=method]').selectOption('bank');
  await pay.getByRole('button', { name: 'Record the payment' }).click();
  await pay.locator('select[name=method]').selectOption('cash');
  await pay.getByRole('button', { name: 'Record the payment' }).click();
  await page.getByRole('link', { name: 'Credit part of it' }).click();
  await page.fill('input[name=qty_2]', '1');
  await page.fill('input[name=reason]', 'Torkarblad byttes inte');
  await page.getByRole('button', { name: 'See what it comes to' }).click();
  await page.getByRole('button', { name: 'Issue the credit note' }).click();

  // The day: sales apart from cash, each a link to its source.
  await page.getByRole('link', { name: 'The day' }).click();
  await expect(page.getByRole('heading', { name: 'Sales' })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Money recorded' })).toBeVisible();
  await expect(page.getByRole('link', { name: /Credit note A-\d+/ }).first()).toBeVisible();
  await expect(page.getByText('It is what was entered, not what the bank says.')).toBeVisible();
  await expect(page.getByRole('link', { name: /[A-Z]+-\d+ · A-\d+/ }).first()).toBeVisible();

  // The bank says something else: a reason is needed.
  const cmp = page.locator('form[action="/day/settlement"]');
  await cmp.locator('select[name=method]').selectOption('bank');
  await cmp.locator('input[name=external]').fill('980');
  await cmp.getByRole('button', { name: 'Record the comparison' }).click();
  await expect(page.getByText('the figures differ; say why')).toBeVisible();
  await page.goBack();
  await cmp.locator('select[name=method]').selectOption('bank');
  await cmp.locator('input[name=external]').fill('980');
  await cmp.locator('input[name=explanation]').fill('Bankavgift drogs före utdraget');
  await cmp.getByRole('button', { name: 'Record the comparison' }).click();
  await expect(page.getByText('Bankavgift drogs före utdraget')).toBeVisible();
  await signOut(page);

  // The owner looks at it and signs it off.
  await signIn(page, owner);
  await page.goto('/day');
  await page.getByRole('button', { name: 'I have looked at this day' }).click();
  await expect(page.getByText('reviewed', { exact: true })).toBeVisible();
  await signOut(page);

  // Money is paid back for the same day afterwards: the sign-off says it is stale.
  await signIn(page, desk);
  await page.goto('/receivables');
  await page.locator('li', { hasText: 'Dagny Dagskassa' }).filter({ has: page.getByRole('link', { name: 'Pay it back' }) }).getByRole('link', { name: 'Pay it back' }).click();
  await page.locator('form:has(input[value=refund]) select[name=method]').selectOption('bank');
  await page.locator('form:has(input[value=refund])').getByRole('button', { name: 'It has been paid out' }).click();
  await page.goto('/day');
  await expect(page.getByText('the day has changed since it was reviewed')).toBeVisible();
  await signOut(page);
});
