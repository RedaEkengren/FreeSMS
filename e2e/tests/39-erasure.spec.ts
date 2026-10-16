import { test, expect } from '@playwright/test';
import { owner, tech, desk, signIn, signOut, getApproved, nameCustomer } from './helpers';

// Somebody asks to be erased. They can be given a copy of what is held first;
// erasing needs a plain acknowledgement and clears the name, telephone, email
// and address; the invoice they were billed keeps the name it went out with,
// because the law requires it; what was cleared and what was kept is written
// down; and it cannot be done twice, or by the shop floor.

test('a customer is given a copy, then erased, and the invoice keeps the name it went out with', async ({ page }) => {
  test.setTimeout(180_000);
  await signIn(page, desk);
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', 'ERA001');
  await page.fill('[name=complaint]', 'Ljud från bakaxeln');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const job = page.url();
  await nameCustomer(page, 'Erik Radering');
  await page.fill('input[form=add-line][name=description]', 'Byte av hjullager');
  await page.fill('input[form=add-line][name=quantity]', '1');
  await page.fill('input[form=add-line][name=unit_price]', '900');
  await page.locator('button[form=add-line]').click();
  await expect(page.locator('table.lines-table tbody tr', { hasText: 'Byte av hjullager' })).toBeVisible();
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

  // The card, with the details a person would ask to have erased.
  await page.goto('/customers?q=Erik+Radering');
  await page.getByRole('link', { name: /Erik Radering/ }).first().click();
  const card = page.url();
  await page.fill('input[name=phone]', '070-555 12 34');
  await page.fill('input[name=email]', 'erik@example.se');
  await page.fill('input[name=address_line1]', 'Lillgatan 3');
  await page.getByRole('button', { name: 'Save the changes' }).click();
  await expect(page.getByText('saved').first()).toBeVisible();

  // A copy of everything held, first.
  const copy = page.getByRole('link', { name: 'Download everything we hold about them' });
  const href = (await copy.getAttribute('href'))!;
  const personID = href.split('/')[2];
  const body = await (await page.request.get(href)).text();
  expect(body).toContain('Erik Radering');
  expect(body).toContain('070-555 12 34');

  // Erasing is behind an acknowledgement, and a refusal to give one stops it.
  await page.locator('details', { hasText: 'Erase this person' }).locator('summary').click();
  const form = page.locator(`form[action="/people/${personID}/erase"]`);
  await form.locator('input[name=reason]').fill('Begäran via e-post');
  await form.getByRole('button', { name: 'Erase' }).click();
  await expect(page).toHaveURL(new RegExp('^' + card.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
  await expect(form.locator('input[type=checkbox]')).toBeVisible();
  await form.locator('input[type=checkbox]').check();
  await form.getByRole('button', { name: 'Erase' }).click();

  // It is written down what was cleared and what was kept, and why.
  await expect(page).toHaveURL(/\/privacy$/);
  const entry = page.locator('ul.lines li', { hasText: 'Begäran via e-post' });
  await expect(entry).toContainText('Cleared:');
  await expect(entry).toContainText('Kept:');
  await expect(entry).toContainText('Issued invoices');

  // The card says so, offers nothing more, and holds none of it.
  await page.goto(card);
  await expect(page.getByText('erased').first()).toBeVisible();
  await expect(page.getByRole('button', { name: 'Erase' })).toHaveCount(0);
  await expect(page.locator('input[name=phone]')).toHaveValue('');
  await expect(page.locator('input[name=email]')).toHaveValue('');
  await expect(page.locator('input[name=address_line1]')).toHaveValue('');
  expect(await (await page.request.get(href)).text()).not.toContain('070-555 12 34');

  // The invoice is what it was: seven years, by law.
  await page.goto(invoice);
  await expect(page.getByText('Erik Radering')).toBeVisible();

  // Not twice, and not from the shop floor.
  // A bare POST is refused for its origin whatever the role, so say where it comes from.
  const from = { Origin: new URL(page.url()).origin };
  const again = await page.request.post(`/people/${personID}/erase`, { form: { reason: 'igen' }, headers: from });
  expect(again.status()).toBe(400);
  await signOut(page);
  await signIn(page, tech);
  expect((await page.request.post(`/people/${personID}/erase`, { form: { reason: 'tekniker' }, headers: from })).status()).toBe(403);
  expect((await page.request.get(href)).status()).toBe(403);
  await signOut(page);
  await signIn(page, owner);
  await page.goto('/privacy');
  await expect(page.locator('ul.lines li', { hasText: 'Begäran via e-post' })).toHaveCount(1);
});
