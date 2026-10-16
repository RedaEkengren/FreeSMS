import { test, expect } from '@playwright/test';
import { desk, tech, signIn, signOut } from './helpers';

// Parts bought outside FreeSMS: the technician asks, the parts desk notes the
// order and moves the day, the part comes in two deliveries (the same note
// entered twice counts once), a wrong one goes back, and the technician sees
// when it is expected but never who from or what it cost.

test('an order noted, delivered in two parts, one wrong part sent back', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page, desk);
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', 'SUP001');
  await page.fill('[name=complaint]', 'Hjullager bak');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const job = page.url();
  await signOut(page);

  await signIn(page, tech);
  await page.goto(job);
  await page.fill('form[action$="/parts"] input[name=description]', 'Hjullager bak höger');
  await page.fill('form[action$="/parts"] input[name=quantity]', '2');
  await page.getByRole('button', { name: 'Ask the parts desk' }).click();
  await signOut(page);

  // The desk notes the order with a day it is expected.
  await signIn(page, desk);
  await page.goto('/supply');
  await expect(page.getByText('Hjullager bak höger')).toBeVisible();
  await page.fill('input[name=supplier]', 'Lagerhuset');
  await page.fill('input[name=reference]', 'LH-26');
  await page.fill('input[name=eta]', '2030-03-04');
  await page.locator('input[type=checkbox][name^=use_]').first().check();
  await page.locator('input[name^=cost_]').first().fill('310,50');
  await page.getByRole('button', { name: 'Note the order' }).click();
  await expect(page.getByRole('heading', { name: /Lagerhuset/ })).toBeVisible();
  const order = page.url();

  // The day moves; one comes, and the same delivery note is entered twice.
  await page.fill('form[action$="/eta"] input[name=eta]', '2030-03-09');
  await page.getByRole('button', { name: 'Change the day' }).click();
  await expect(page.getByText('Expected:')).toContainText('→');
  const receive = page.locator('form[action$="/receive"]');
  await receive.locator('input[name=quantity]').fill('1');
  await receive.locator('input[name=delivery_ref]').fill('DN-26');
  await receive.locator('select[name=route]').selectOption('direct');
  await receive.getByRole('button', { name: 'Book the delivery' }).click();
  await receive.locator('input[name=quantity]').fill('1');
  await receive.locator('input[name=delivery_ref]').fill('DN-26');
  await receive.locator('select[name=route]').selectOption('direct');
  await receive.getByRole('button', { name: 'Book the delivery' }).click();
  await expect(page.getByText('1 still owed')).toBeVisible();

  // The second comes, and it is the wrong part: it goes back, and the job asks again.
  await receive.locator('input[name=quantity]').fill('1');
  await receive.locator('input[name=delivery_ref]').fill('DN-27');
  await receive.locator('select[name=route]').selectOption('direct');
  await receive.getByRole('button', { name: 'Book the delivery' }).click();
  await expect(page.getByText('all here')).toBeVisible();
  const back = page.locator('details', { hasText: 'Send some of it back' }).last();
  await back.locator('summary').click();
  await back.locator('select[name=reason]').selectOption('wrong_part');
  await back.locator('input[name=replacement]').check();
  await back.getByRole('button', { name: 'Plan the return' }).click();
  await page.goto('/supply');
  await expect(page.getByRole('heading', { name: 'Going back' })).toBeVisible();
  await expect(page.getByText('to send back')).toBeVisible();
  await page.getByRole('button', { name: 'It has been sent back' }).click();
  await page.fill('input[name=credited]', '310,50');
  await page.getByRole('button', { name: 'The supplier will credit it' }).click();
  await expect(page.getByRole('heading', { name: 'Going back' })).toHaveCount(0);
  await expect(page.getByText('Replacement: Hjullager bak höger')).toBeVisible();
  await signOut(page);

  // The technician sees the replacement is not ordered yet, and nothing of the supplier.
  await signIn(page, tech);
  await page.goto(job);
  await expect(page.locator('#live-requests')).toContainText('not ordered yet');
  await expect(page.locator('main')).not.toContainText('Lagerhuset');
  await expect(page.locator('main')).not.toContainText('310');
  await signOut(page);
  expect(order).toContain('/supply/orders/');
});
