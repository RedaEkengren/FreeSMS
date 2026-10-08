import { test, expect } from '@playwright/test';
import { tech, desk, signIn, pricedJob } from './helpers';

// Two people, two screens, one job, and nobody reloads. The technician
// reports something from the pit; the front desk's job page and board show
// it while they are open. A colleague adds a line to the same job, and the
// table the front desk is halfway through typing into waits until they leave
// it, rather than taking what they typed.

test('a screen brings itself up to date, and leaves alone what somebody is typing', async ({ browser }) => {
  const deskCtx = await browser.newContext();
  const techCtx = await browser.newContext();
  try {
    const d = await deskCtx.newPage();
    await signIn(d, desk);
    const job = await pricedJob(d, 'LIV123');

    const board = await deskCtx.newPage();
    await board.goto('/board');
    const card = board.locator('a.card', { hasText: /LIV\s?123/ });
    await expect(card).toBeVisible();
    await expect(card).not.toContainText('noticed');

    // Halfway through a line, cursor still in the field.
    await d.goto(job);
    const what = d.locator('input[form=add-line][name=description]');
    await what.fill('Halvskrivet');
    await what.focus();
    // Set on the page as it is now; a reload would lose it.
    await d.evaluate(() => { (window as any).__notReloaded = true; });
    await board.evaluate(() => { (window as any).__notReloaded = true; });

    const t = await techCtx.newPage();
    await signIn(t, tech);
    await t.goto(job);
    const finding = t.locator('form[action$="/findings"]');
    await finding.locator('input[name=note]').fill('Spindelled vänster glapp');
    await finding.getByRole('button', { name: 'Tell the front desk' }).click();

    await expect(d.locator('#live-findings')).toContainText('Spindelled vänster glapp');
    await expect(card).toContainText('1 thing noticed, not priced');

    // Somebody else at the counter adds a line to the same job: the lines
    // table changed, and the one being typed into is in it.
    // A browser of its own: drafts are kept per browser, and this is
    // somebody else's.
    const otherCtx = await browser.newContext();
    const other = await otherCtx.newPage();
    await signIn(other, desk);
    await other.goto(job);
    await other.fill('input[form=add-line][name=description]', 'Andra raden');
    await other.fill('input[form=add-line][name=unit_price]', '200');
    await other.locator('button[form=add-line]').click();
    await expect(other.locator('table.lines-table')).toContainText('Andra raden');
    await otherCtx.close();

    // The totals beside it came through -- 500 and 200 net, 875 with VAT --
    // and the table being typed in waits.
    await expect(d.locator('#live-totals')).toContainText('875');
    await expect(what).toHaveValue('Halvskrivet');
    await expect(what).toBeFocused();
    await expect(d.locator('table.lines-table')).not.toContainText('Andra raden');

    // Given up on, and left: now it catches up.
    await what.fill('');
    await what.blur();
    await expect(d.locator('table.lines-table')).toContainText('Andra raden');
    expect(await d.evaluate(() => (window as any).__notReloaded)).toBe(true);
    expect(await board.evaluate(() => (window as any).__notReloaded)).toBe(true);
  } finally {
    await deskCtx.close();
    await techCtx.close();
  }
});
