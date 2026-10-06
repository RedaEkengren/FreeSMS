import { test, expect, Page } from '@playwright/test';
import { desk, signIn } from './helpers';

// Money arriving against an invoice, recorded at the counter. The invoice
// from the surcharges test, 1 823,29, is owed in full when this starts.
async function openOwed(page: Page) {
  await page.goto('/receivables');
  await page.getByRole('link', { name: /Karl Kostnad/ }).click();
  await expect(page).toHaveURL(/\/invoices\//);
}

test.describe.serial('getting paid', () => {
  test('part paid, the rest in cash rounded at the counter, and settled', async ({ page }) => {
    await signIn(page, desk);
    await openOwed(page);
    const form = page.locator('form[action$="/payments"]');

    await form.locator('input[name=amount]').fill('1000');
    await form.locator('select[name=method]').selectOption('bank');
    await form.getByRole('button', { name: 'Record the payment' }).click();
    await expect(page.locator('section dl')).toContainText('823.29');

    // Empty: whatever is owed. In cash: 823,00 and 0,29 rounding.
    await form.locator('select[name=method]').selectOption('cash');
    await form.getByRole('button', { name: 'Record the payment' }).click();
    await expect(page.getByText('settled')).toBeVisible();
    await expect(page.getByText(/823\.00.*0\.29 rounding/)).toBeVisible();

    await page.goto('/receivables');
    await expect(page.getByText('Karl Kostnad')).toHaveCount(0);
  });

  test('a payment recorded in error is reversed, not deleted', async ({ page }) => {
    await signIn(page, desk);
    await page.goto('/receivables');
    // Settled, so not listed: reach it through the job.
    await page.goto('/board');
    await page.locator('.card', { hasText: 'CHG001' }).click();
    await page.getByRole('link', { name: /A-\d+/ }).first().click();

    await page.getByRole('button', { name: 'Recorded in error: reverse it' }).last().click();
    await expect(page.getByText('reverses a payment recorded in error')).toBeVisible();
    await expect(page.locator('section dl')).toContainText('823.29');
    // Both rows stay: the mistake and its correction.
    await expect(page.locator('section ul.lines > li')).toHaveCount(3);
    await openOwed(page);
  });
});
