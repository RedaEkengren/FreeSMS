import { test, expect } from '@playwright/test';
import { tech, desk, signIn, signOut, leave, readIn, collectAlerts, pricedJob, shopLanguage } from './helpers';

// The marketing page's first scene, performed against the real product.
// site/src/index.html links this test, and the site generator refuses to
// write the scene if it is missing: a scene shows what this does, in the
// words this reads, or it is not shown.
//
// The people read Swedish, as the scene does, so the words checked here are
// the scene's words.

test.describe.serial('the wifi scene', () => {
  test('the wifi goes down in the pit, and the front desk still hears about it', async ({ page, context, browser }) => {
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
      // A Swedish workshop: the sign-in page speaks the workshop's language.
      await shopLanguage(browser, 'sv');
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

      // A locked screen, a closed tab: it waits in the phone -- long enough
      // for the session to run out.
      await phone.close();
      await context.clearCookies();
      await context.setOffline(false);
      phone = await context.newPage();
      await phone.goto(job);
      // The phone opens on the sign-in page and sends nothing until the
      // same person is back. It says nothing about the work there either:
      // nobody is signed in, and on a shared phone it may not be theirs.
      await expect(phone).toHaveURL(/\/login/);
      await expect(phone.locator('#held')).toBeHidden();
      await signIn(phone, tech);
      await phone.goto(job);
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
      await shopLanguage(browser, 'en');
      await context.setOffline(false);
      await readIn(phone, '');
      await leave(phone);
      await signIn(phone, tech);
      await readIn(phone, '');
    }
  });
});
