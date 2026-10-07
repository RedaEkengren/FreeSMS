import { test, expect } from '@playwright/test';
import { owner, desk, signIn, signOut, collectAlerts, pricedJob, lineCount } from './helpers';

// keep.js, in a browser: what is typed survives, and what is sent with no
// connection is held, sent once when the connection is back, and never sent
// as somebody else.
test.describe.serial('keeping work', () => {
  test('a job line typed is kept, from the fields that sit outside its form', async ({ page }) => {
    await signIn(page, desk);
    const job = await pricedJob(page, 'KEP001');
    await page.fill('input[form=add-line][name=description]', 'Halvskriven rad');
    await page.fill('input[form=add-line][name=quantity]', '2,5');
    await page.reload();
    await expect(page.locator('input[form=add-line][name=description]')).toHaveValue('Halvskriven rad');
    await expect(page.locator('input[form=add-line][name=quantity]')).toHaveValue('2,5');
    expect(job).toContain('/jobs/');
  });

  test('a line added with no connection is held, and sent once when it is back', async ({ page, context }) => {
    const alerts = collectAlerts(page);
    await signIn(page, desk);
    const job = await pricedJob(page, 'OFF001');

    await context.setOffline(true);
    await page.fill('input[form=add-line][name=description]', 'Lagt till utan nät');
    await page.fill('input[form=add-line][name=quantity]', '1');
    await page.fill('input[form=add-line][name=unit_price]', '100');
    await page.locator('button[form=add-line]').click();
    await expect.poll(() => alerts.join(' ')).toContain('being held');
    await expect(page.locator('#held')).toContainText('waiting to be sent');

    await context.setOffline(false);
    await page.evaluate(() => window.dispatchEvent(new Event('online')));
    await expect(page.locator('#held')).toBeHidden();
    expect(await lineCount(page, job, 'Lagt till utan nät')).toBe(1);
  });

  test('held work survives the session ending, and is sent once after signing in again', async ({ page, context }) => {
    collectAlerts(page);
    await signIn(page, desk);
    const job = await pricedJob(page, 'OFF002');

    await context.setOffline(true);
    await page.fill('input[form=add-line][name=description]', 'Hölls över utloggning');
    await page.fill('input[form=add-line][name=unit_price]', '100');
    await page.locator('button[form=add-line]').click();
    await expect(page.locator('#held')).toContainText('waiting to be sent');

    // The session ends while the work is held: the cookie is gone.
    await context.clearCookies();
    await context.setOffline(false);
    await page.evaluate(() => window.dispatchEvent(new Event('online')));
    await expect(page.locator('#held')).toContainText('sign in again');

    await signIn(page, desk);
    await expect.poll(async () => lineCount(page, job, 'Hölls över utloggning')).toBe(1);
  });

  // The words are the page's, so a Swedish technician reads Swedish at the
  // moment that matters -- the connection gone and their work held -- and
  // not English in the middle of a Swedish screen.
  test('held work is talked about in the language the person reads', async ({ page, context }) => {
    const alerts = collectAlerts(page);
    await signIn(page, desk);
    const job = await pricedJob(page, 'SPR001');
    await page.goto('/account');
    await page.selectOption('select[name=locale]', 'sv');
    await page.locator('form[action="/account/language"] button').click();

    try {
      await page.goto(job);
      await context.setOffline(true);
      await page.fill('input[form=add-line][name=description]', 'Hålls på svenska');
      await page.fill('input[form=add-line][name=quantity]', '1');
      await page.fill('input[form=add-line][name=unit_price]', '100');
      await page.locator('button[form=add-line]').click();
      await expect.poll(() => alerts.join(' ')).toContain('Ingen anslutning. Det här hålls');
      await expect(page.locator('#held')).toHaveText('1 sak väntar på att skickas.');

      await context.setOffline(false);
      await page.evaluate(() => window.dispatchEvent(new Event('online')));
      await expect(page.locator('#held')).toBeHidden();
      expect(await lineCount(page, job, 'Hålls på svenska')).toBe(1);
    } finally {
      // The rest of the suite reads English.
      await context.setOffline(false);
      await page.goto('/account');
      await page.selectOption('select[name=locale]', '');
      await page.locator('form[action="/account/language"] button').click();
    }
  });

  test('a draft stays with the person who typed it on a shared browser', async ({ page }) => {
    await signIn(page, desk);
    await page.goto('/jobs/new');
    await page.fill('[name=complaint]', 'Disas halvskrivna intag');
    // Give the autosave its moment to reach the server.
    await page.waitForTimeout(1500);
    await signOut(page);

    await signIn(page, owner);
    await page.goto('/jobs/new');
    await page.waitForTimeout(500);
    await expect(page.locator('[name=complaint]')).toHaveValue('');
    await signOut(page);

    await signIn(page, desk);
    await page.goto('/jobs/new');
    await expect(page.locator('[name=complaint]')).toHaveValue('Disas halvskrivna intag');
  });
});
