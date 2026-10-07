import { test, expect } from '@playwright/test';
import { desk, tech, signIn, signOut, leave, readIn, shopClock } from './helpers';

// The marketing page's "when is it ready?" scene, performed against the real
// product, in Swedish as the scene is. site/src/index.html links this test.
//
// The scene lets time pass with the technician's clock standing still. A
// test cannot wait an hour and a half, so it moves the promise closer
// instead: the verdict compares the work left with the time left, and
// either way two hours of work now has an hour and forty minutes.
test.describe.serial('the ready scene', () => {
  test('a promise that the work left cannot meet is flagged before it is broken', async ({ page }) => {
    await signIn(page, desk);
    await page.goto('/jobs/new');
    await page.fill('input[name=registration]', 'DEF456');
    await page.fill('[name=complaint]', 'Kamremsbyte');
    await page.getByRole('button', { name: 'Open the job' }).click();
    await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
    const job = page.url();
    await page.fill('input[form=add-line][name=description]', 'Byte av kamrem');
    await page.fill('input[form=add-line][name=quantity]', '3');
    await page.fill('input[form=add-line][name=unit_price]', '895');
    await page.locator('button[form=add-line]').click();
    for (const action of ['Price it', 'Ask the customer', 'Mark it approved']) {
      await page.getByRole('button', { name: action }).click();
    }
    await signOut(page);

    // The technician starts it, as Erik does at twelve.
    await signIn(page, tech);
    await page.goto(job);
    await page.getByRole('button', { name: 'Start work' }).click();
    await signOut(page);
    await signIn(page, desk);

    try {
      await readIn(page, 'sv');
      await page.goto(job);

      // Three hours of work, promised with time to spare.
      await page.fill('input[name=promised_at]', await shopClock(page, 190));
      await page.getByRole('button', { name: 'Spara löftet' }).click();
      const side = page.locator('.side');
      await expect(side.getByText('Under arbete')).toBeVisible();
      await expect(side.getByText('När den blir klar')).toBeVisible();
      await expect(side.locator('.tag', { hasText: 'i tid' })).toBeVisible();
      await expect(side.getByText('0,0 av 3,0 h')).toBeVisible();
      // The promise is written in Swedish too: "tis 6 okt 16:00".
      await expect(side.locator('dt', { hasText: 'Utlovad' }).locator('+ dd')).toHaveText(/^(mån|tis|ons|tor|fre|lör|sön) \d{1,2} (jan|feb|mar|apr|maj|jun|jul|aug|sep|okt|nov|dec) \d\d:\d\d/);

      // Nothing clocked, and an hour and forty minutes left to a promise
      // three hours of work cannot meet: flagged now, not at four o'clock.
      await page.fill('input[name=promised_at]', await shopClock(page, 100));
      await page.getByRole('button', { name: 'Spara löftet' }).click();
      await expect(side.locator('.tag', { hasText: 'i farozonen' })).toBeVisible();
      await expect(side.getByText('Kvar')).toBeVisible();

      await page.goto('/board');
      const card = page.locator('a.card', { hasText: /DEF\s?456/ });
      await expect(card).toContainText('utlovad');
      await expect(card).toContainText('0,0 av 3,0 h');
      await expect(card.locator('.tag', { hasText: 'i farozonen' })).toBeVisible();
    } finally {
      await readIn(page, '');
      await leave(page);
    }
  });
});
