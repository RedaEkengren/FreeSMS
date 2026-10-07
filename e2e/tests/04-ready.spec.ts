import { test, expect } from '@playwright/test';
import { desk, signIn, shopClock } from './helpers';

// "When is it ready?", answered on the job and on the board.
test.describe.serial('when it will be ready', () => {
  test('two hours of work promised in one is at risk, on the job and on the board', async ({ page }) => {
    await signIn(page, desk);
    await page.goto('/jobs/new');
    await page.fill('input[name=registration]', 'RDY001');
    await page.fill('[name=complaint]', 'Kamrem');
    await page.getByRole('button', { name: 'Open the job' }).click();
    await page.fill('input[form=add-line][name=description]', 'Byte av kamrem');
    await page.fill('input[form=add-line][name=quantity]', '2');
    await page.fill('input[form=add-line][name=unit_price]', '895');
    await page.locator('button[form=add-line]').click();

    const promise = await shopClock(page, 60);
    await page.fill('input[name=promised_at]', promise);
    await page.getByRole('button', { name: 'Save the promise' }).click();

    // The control shows back exactly the wall clock that was typed.
    await expect(page.locator('input[name=promised_at]')).toHaveValue(promise);
    await expect(page.getByText('0.0 of 2.0 h')).toBeVisible();
    await expect(page.locator('.side .tag.warranty', { hasText: 'at risk' })).toBeVisible();
    await expect(page.locator('meter.progress')).toHaveAttribute('high', '120');

    await page.goto('/board');
    const card = page.locator('.card', { hasText: 'RDY001' });
    await expect(card.getByText('at risk')).toBeVisible();
  });

  test('a job with no labour line says it has no estimate', async ({ page }) => {
    await signIn(page, desk);
    await page.goto('/jobs/new');
    await page.fill('input[name=registration]', 'RDY002');
    await page.fill('[name=complaint]', 'Titta på ljudet');
    await page.getByRole('button', { name: 'Open the job' }).click();
    await expect(page.getByText('No estimate: there is no labour line on the job yet.')).toBeVisible();
    await expect(page.locator('meter.progress')).toHaveCount(0);
  });
});
