import { test, expect, Page, BrowserContext } from '@playwright/test';
import { tech, desk, signIn, pricedJob } from './helpers';

// What is waiting for each person arrives on their screen by itself, and
// leaves it when somebody deals with it. The site's "For me" scene shows
// these steps; site/src/index.html links this test.
//
// The parts desk's person was added by the whole-job scene.

const parts = { email: 'parts@e2e.test', password: 'lagrets lösenfras 1' };

async function as(ctx: BrowserContext, who: { email: string; password: string }): Promise<Page> {
  const page = await ctx.newPage();
  await signIn(page, who);
  return page;
}

test('what is waiting for each person arrives by itself and leaves when it is dealt with', async ({ browser }) => {
  const deskCtx = await browser.newContext();
  const techCtx = await browser.newContext();
  const partsCtx = await browser.newContext();
  try {
    const d = await as(deskCtx, desk);
    const job = await pricedJob(d, 'FOR123');
    for (const action of ['Price it', 'Ask the customer', 'Mark it approved']) {
      await d.getByRole('button', { name: action }).click();
    }

    // The technician asks for a part, and keeps their list open.
    const t = await as(techCtx, tech);
    await t.goto(job);
    await t.getByRole('button', { name: 'Start work' }).click();
    await t.fill('form[action$="/parts"] input[name=description]', 'Styrled vänster');
    await t.getByRole('button', { name: 'Ask the parts desk' }).click();
    // Earlier scenes leave real things on the list; this one is about FOR 123.
    const mine = await techCtx.newPage();
    await mine.goto('/mine');
    const ours = mine.locator('#live-mine li', { hasText: /FOR\s?123/ });
    await expect(ours).toHaveCount(0);
    await expect(mine.locator('#mine-count')).toHaveAttribute('data-n', /\d+/);
    const before = Number(await mine.locator('#mine-count').getAttribute('data-n'));
    await mine.evaluate(() => { (window as any).__notReloaded = true; });

    // The parts desk books it in.
    const p = await as(partsCtx, parts);
    await p.goto('/parts');
    await p.locator('.card', { hasText: 'Styrled vänster' }).getByRole('button', { name: 'It has arrived' }).click();

    // It arrives on the technician's list, and in the count, by itself.
    await expect(ours).toContainText('The part you asked for has arrived: Styrled vänster');
    await expect(mine.locator('#mine-count')).toHaveAttribute('data-n', String(before + 1));

    // Back on the job is dealing with it.
    await t.goto(job);
    await t.getByRole('button', { name: 'Start clock' }).click();
    await expect(t.getByRole('button', { name: 'Stop clock' })).toBeVisible();
    await expect(ours).toHaveCount(0);
    await expect(mine.locator('#mine-count')).toHaveAttribute('data-n', String(before));
    expect(await mine.evaluate(() => (window as any).__notReloaded)).toBe(true);

    // Something found under the car waits for the front desk ...
    const deskMine = await deskCtx.newPage();
    await deskMine.goto('/mine');
    await deskMine.evaluate(() => { (window as any).__notReloaded = true; });
    await t.fill('form[action$="/findings"] input[name=note]', 'Spindelled glapp');
    await t.getByRole('button', { name: 'Tell the front desk' }).click();
    const item = deskMine.locator('#live-mine li', { hasText: /FOR\s?123/ });
    await expect(item).toContainText('1 thing noticed, not priced');
    // ... with nothing of the customer on the technician's.
    await expect(mine.locator('#live-mine')).not.toContainText('Spindelled glapp');

    // ... until somebody at the counter deals with it.
    await d.goto(job);
    await d.getByRole('button', { name: 'Dealt with' }).click();
    await expect(item).toHaveCount(0);
    expect(await deskMine.evaluate(() => (window as any).__notReloaded)).toBe(true);
  } finally {
    await deskCtx.close();
    await techCtx.close();
    await partsCtx.close();
  }
});
