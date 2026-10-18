import { test, expect, Page } from '@playwright/test';
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

// Takes a car in for a new customer, works it to ready, and leaves the front desk signed in.
async function readyJob(page: Page, registration: string, customer: string): Promise<string> {
  await signIn(page, desk);
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', registration);
  await page.fill('[name=complaint]', 'Service');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const job = page.url();
  await nameCustomer(page, customer);
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
  await signIn(page, desk);
  return job;
}

test('an invoice that was issued without the details says what it lacks, and one with them is written as an electronic invoice', async ({ page }) => {
  test.setTimeout(240_000);

  // The workshop says who it is and how it is paid.
  await signIn(page, owner);
  await page.goto('/shop');
  await page.fill('input[name=address_line1]', 'Verkstadsgatan 1');
  await page.fill('input[name=postal_code]', '123 45');
  await page.fill('input[name=city]', 'Stockholm');
  await page.fill('input[name=org_number]', '556677-8899');
  await page.fill('input[name=vat_number]', 'SE556677889901');
  await page.fill('input[name=payment_reference]', 'Bankgiro 123-4567');
  await page.selectOption('select[name=payment_scheme]', 'bankgiro');
  await page.fill('input[name=endpoint_id]', '0007:5566778899');
  await page.getByRole('button', { name: 'Save' }).first().click();
  await signOut(page);

  // Issued before the customer's card says where they receive invoices.
  const lacking = await readyJob(page, 'EIN002', 'Lena Saknas');
  await page.goto(lacking);
  await page.getByRole('button', { name: 'Invoice it' }).click();
  await page.getByRole('link', { name: /A-\d+/ }).first().click();
  const lackingInvoice = page.url();
  await page.getByRole('link', { name: 'Download the electronic invoice' }).click();
  await expect(page.getByRole('heading', { name: 'Electronic invoice' })).toBeVisible();
  await expect(page.getByText("the customer's electronic invoice address")).toBeVisible();
  await expect(page.getByText("the customer's reference")).toBeVisible();
  await expect(page.getByText("the workshop's company number")).toHaveCount(0);
  const refused = await page.request.get(lackingInvoice + '/einvoice.xml');
  expect(refused.status()).toBe(409);
  await signOut(page);

  // The customer's card is complete before this one is issued.
  const whole = await readyJob(page, 'EIN003', 'Maja Komplett');
  await page.goto('/customers?q=Maja+Komplett');
  await page.getByRole('link', { name: /Maja Komplett/ }).first().click();
  await page.fill('input[name=address_line1]', 'Kundgatan 2');
  await page.fill('input[name=postal_code]', '111 22');
  await page.fill('input[name=city]', 'Göteborg');
  await page.fill('input[name=country]', 'SE');
  await page.fill('input[name=buyer_reference]', 'Avd 4, Anna A');
  await page.fill('input[name=endpoint_id]', '0007:1122334455');
  await page.getByRole('button', { name: 'Save the changes' }).click();
  await expect(page.getByText('saved').first()).toBeVisible();
  await page.goto(whole);
  await page.getByRole('button', { name: 'Invoice it' }).click();
  await page.getByRole('link', { name: /A-\d+/ }).first().click();
  const invoice = page.url();
  const file = await page.request.get(invoice + '/einvoice.xml');
  expect(file.status()).toBe(200);
  expect(file.headers()['content-type']).toContain('application/xml');
  expect(file.headers()['content-disposition']).toMatch(/attachment; filename="A-\d+\.xml"/);
  const xml = await file.text();
  expect(xml).toContain('urn:fdc:peppol.eu:2017:poacc:billing:01:1.0');
  expect(xml).toContain('<cbc:BuyerReference>Avd 4, Anna A</cbc:BuyerReference>');
  expect(xml).toContain('<cbc:EndpointID schemeID="0007">1122334455</cbc:EndpointID>');
  expect(xml).toContain('SE:BANKGIRO');
  expect(xml).toContain('<cbc:ID>1234567</cbc:ID>');
  expect(xml).toMatch(/unitCode="(C62|HUR)"/);
  await signOut(page);

  // The shop floor has no business with it.
  await signIn(page, tech);
  const forbidden = await page.request.get(invoice + '/einvoice.xml');
  expect(forbidden.status()).toBe(403);
  await signOut(page);
});
