import { test, expect } from '@playwright/test';
import { owner, desk, tech, signIn, signOut, nameCustomer } from './helpers';

// Förbrukningsmaterial and the invoicing fee, set by the owner, shown on the
// job with the total, and on the invoice the same.
test.describe.serial('what an invoice adds', () => {
  test('the owner sets the surcharges', async ({ page }) => {
    await signIn(page, owner);
    await page.goto('/shop');
    const form = page.locator('form[action="/shop/charges"]');
    await form.locator('input[name=consumables_percent]').fill('5');
    await form.locator('input[name=consumables_cap]').fill('500');
    await form.locator('input[name=invoice_fee]').fill('49');
    await form.getByRole('button', { name: 'Save' }).click();
    await expect(form.locator('input[name=consumables_percent]')).toHaveValue('5.0');
  });

  test('the job shows them with its total, and the invoice prints the same', async ({ page }) => {
    await signIn(page, desk);
    await page.goto('/jobs/new');
    await page.fill('input[name=registration]', 'CHG001');
    await page.fill('[name=complaint]', 'Kamrem');
    await page.getByRole('button', { name: 'Open the job' }).click();
    const job = page.url();
    await nameCustomer(page, 'Karl Kostnad');
    await page.fill('input[form=add-line][name=description]', 'Byte av kamrem');
    await page.fill('input[form=add-line][name=quantity]', '1,5');
    await page.fill('input[form=add-line][name=unit_price]', '895');
    await page.locator('button[form=add-line]').click();

    // 5 per cent of 1 342,50 is 67,13; the fee 49,00; 1 458,63 net and
    // 1 823,29 with VAT.
    const totals = page.locator('dl.totals');
    await expect(totals).toContainText('Consumables, 5.0% of the labour');
    await expect(totals).toContainText('67.13');
    await expect(totals).toContainText('Invoicing fee');
    await expect(totals).toContainText(/1,823\.29/);
    for (const action of ['Price it', 'Ask the customer', 'Mark it approved']) {
      await page.getByRole('button', { name: action }).click();
    }
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
    await expect(page.getByText('Consumables, 5.0% of the labour')).toBeVisible();
    await expect(page.getByText('Invoicing fee')).toBeVisible();
    await expect(page.getByText(/1,823\.29/)).toBeVisible();
  });
});
