import { test, expect, Page } from '@playwright/test';
import { tech, desk, signIn, signOut, collectAlerts, pricedJob } from './helpers';

// The marketing page's first scene, performed against the real product.
// site/src/index.html links this test, and the site generator refuses to
// write the scene if it is missing: a scene shows what this does, in the
// words this reads, or it is not shown.
//
// The people read Swedish, as the scene does, so the words checked here are
// the scene's words.

// Signing out in whatever language the page is in.
async function leave(page: Page) {
  await page.locator('form[action="/logout"] button').click();
  await expect(page).toHaveURL(/\/login/);
}

async function readIn(page: Page, locale: string) {
  await page.goto('/account');
  await page.selectOption('select[name=locale]', locale);
  await page.locator('form[action="/account/language"] button').click();
}

test.describe.serial('the wifi scene', () => {
  test('the wifi goes down in the pit, and the front desk still hears about it', async ({ page, context }) => {
    // The car is in, priced and approved, and the technician is on it with
    // the clock running.
    await signIn(page, desk);
    const job = await pricedJob(page, 'WIF123');
    for (const action of ['Price it', 'Ask the customer', 'Mark it approved']) {
      await page.getByRole('button', { name: action }).click();
    }
    await signOut(page);
    await signIn(page, tech);
    await page.goto(job);
    await page.getByRole('button', { name: 'Start work' }).click();
    await page.getByRole('button', { name: 'Start clock' }).click();

    let phone = page;
    try {
      await readIn(phone, 'sv');
      await phone.goto(job);
      await expect(phone.getByText('Under arbete').first()).toBeVisible();
      await expect(phone.getByRole('button', { name: 'Stoppa klockan' })).toBeVisible();
      await expect(phone.getByRole('heading', { name: 'Lämna över' })).toBeVisible();

      // Something else needs doing, and the wifi does not reach the pit.
      const alerts = collectAlerts(phone);
      await context.setOffline(true);
      const finding = phone.locator('form[action$="/findings"]');
      await expect(finding.getByText('Något mer behöver göras')).toBeVisible();
      await finding.locator('input[name=note]').fill('Bromsbelägg bak nere på 2 mm');
      await finding.getByRole('button', { name: 'Säg till framdisken' }).click();
      await expect.poll(() => alerts.join(' ')).toBe('Ingen anslutning. Det här hålls och skickas när det finns en anslutning.');
      await expect(phone.locator('#held')).toHaveText('1 sak väntar på att skickas.');

      // A locked screen, a closed tab: it waits in the phone.
      await phone.close();
      await context.setOffline(false);
      phone = await context.newPage();
      await phone.goto(job);
      // Opened with a connection, the phone sends what it held.
      await expect(phone.locator('#held')).toBeHidden();
      await leave(phone);

      // The front desk reloads the board and sees it, once.
      await signIn(phone, desk);
      await readIn(phone, 'sv');
      await phone.goto('/board');
      const card = phone.locator('a.card', { hasText: /WIF\s?123/ });
      await expect(card).toContainText('1 sak upptäckt, inte prissatt');
      await expect(card).toContainText(`${tech.name} jobbar på den`);
    } finally {
      // The rest of the suite reads English.
      await context.setOffline(false);
      await readIn(phone, '');
      await leave(phone);
      await signIn(phone, tech);
      await readIn(phone, '');
    }
  });
});
