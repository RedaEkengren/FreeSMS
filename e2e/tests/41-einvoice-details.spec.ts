import { test, expect } from '@playwright/test';
import { owner, tech, desk, signIn, signOut, getApproved, nameCustomer } from './helpers';

// The facts an electronic invoice needs are asked for and written onto the
// invoice when it is issued, because an issued invoice cannot be given them
// afterwards. The workshop says how it is paid and where it receives invoices;
// the customer's card says their country, their reference and where they
// receive invoices; the invoice carries the reference, and keeps it when the
// card is changed later. Nothing is sent electronically yet.

test('the details an electronic invoice needs are asked for and frozen onto the invoice', async ({ page }) => {
  test.setTimeout(180_000);

  await signIn(page, owner);
  await page.goto('/shop');
  await page.selectOption('select[name=payment_scheme]', 'bankgiro');
  await page.fill('input[name=endpoint_id]', '0007:5566778899');
  await page.getByRole('button', { name: 'Save' }).first().click();
  await page.goto('/shop');
  await expect(page.locator('select[name=payment_scheme]')).toHaveValue('bankgiro');
  await expect(page.locator('input[name=endpoint_id]')).toHaveValue('0007:5566778899');
  await signOut(page);

  await signIn(page, desk);
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', 'EIN001');
  await page.fill('[name=complaint]', 'Service');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const job = page.url();
  await nameCustomer(page, 'Elin Referens');
  await page.fill('input[form=add-line][name=description]', 'Oljebyte');
  await page.fill('input[form=add-line][name=quantity]', '1');
  await page.fill('input[form=add-line][name=unit_price]', '600');
  await page.locator('button[form=add-line]').click();
  await expect(page.locator('table.lines-table tbody tr', { hasText: 'Oljebyte' })).toBeVisible();
  await getApproved(page);
  await signOut(page);
  await signIn(page, tech);
  await page.goto(job);
  await page.getByRole('button', { name: 'Start work' }).click();
  await page.getByRole('button', { name: 'It is ready' }).click();
  await signOut(page);

  // The customer's card, before the invoice: a country that is not two letters is refused, and
  // a lower-case one is written the way it is printed.
  await signIn(page, desk);
  await page.goto('/customers?q=Elin+Referens');
  await page.getByRole('link', { name: /Elin Referens/ }).first().click();
  const card = page.url();
  await page.fill('input[name=country]', 'S1');
  await page.getByRole('button', { name: 'Save the changes' }).click();
  await expect(page.getByText('two letters')).toBeVisible();
  await page.goto(card);
  await page.fill('input[name=country]', 'se');
  await page.fill('input[name=buyer_reference]', 'Avd 4, Anna A');
  await page.fill('input[name=endpoint_id]', '0007:1122334455');
  await page.getByRole('button', { name: 'Save the changes' }).click();
  await expect(page.getByText('saved').first()).toBeVisible();
  await expect(page.locator('input[name=country]')).toHaveValue('SE');

  // Issued: the invoice prints the reference they asked for.
  await page.goto(job);
  await page.getByRole('button', { name: 'Invoice it' }).click();
  await page.getByRole('link', { name: /A-\d+/ }).first().click();
  const invoice = page.url();
  await expect(page.getByText('Your reference Avd 4, Anna A')).toBeVisible();

  // The card is changed afterwards; the invoice that was sent is not.
  await page.goto(card);
  await page.fill('input[name=buyer_reference]', 'Avd 9, Bo B');
  await page.getByRole('button', { name: 'Save the changes' }).click();
  await expect(page.locator('input[name=buyer_reference]')).toHaveValue('Avd 9, Bo B');
  await page.goto(invoice);
  await expect(page.getByText('Your reference Avd 4, Anna A')).toBeVisible();
  await expect(page.getByText('Avd 9, Bo B')).toHaveCount(0);
  await signOut(page);
});
