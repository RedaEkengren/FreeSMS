import { test, expect, Page, BrowserContext } from '@playwright/test';
import { owner, tech, desk, signIn } from './helpers';

// The rota: whoever plans staff sets Erik's week and moves a day; the law's
// rest is warned about, not enforced; Erik is told, and reads it; the front
// desk sees that Erik is away on a day and not why. The site's rota scene
// shows these steps; site/src/index.html links this test.

async function as(ctx: BrowserContext, who: { email: string; password: string }): Promise<Page> {
  const page = await ctx.newPage();
  await signIn(page, who);
  return page;
}

// The Monday after next, and that week's days, in the shop's calendar.
async function nextWeek(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const d = new Date(Date.now() + 7 * 86_400_000);
    while (d.getDay() !== 1) d.setDate(d.getDate() + 1);
    return [0, 1, 2, 3, 4].map((i) => {
      const x = new Date(d);
      x.setDate(d.getDate() + i);
      return new Intl.DateTimeFormat('sv-SE').format(x);
    });
  });
}

test('a week is planned, a short rest is warned about, and the person is told', async ({ browser }) => {
  const ownerCtx = await browser.newContext();
  const techCtx = await browser.newContext();
  const deskCtx = await browser.newContext();
  try {
    const o = await as(ownerCtx, owner);
    const [mon, , wed, thu, fri] = await nextWeek(o);

    // Erik's usual week: seven to four, Monday to Friday.
    await o.goto('/rota');
    const rota = o.locator('form[action="/rota"]:has(input[name=action][value=rota])');
    await rota.locator('select[name=user_id]').selectOption({ label: tech.name });
    await rota.locator('input[name=date]').fill(mon);
    for (let d = 1; d <= 5; d++) {
      await rota.locator(`input[name=h0${d}_from]`).fill('07:00');
      await rota.locator(`input[name=h0${d}_to]`).fill('16:00');
    }
    await rota.getByRole('button', { name: 'Save the rota' }).click();

    // Thursday becomes a late shift: eight hours before Friday's seven.
    const change = o.locator('form[action="/rota"]:has(button[value=shift])');
    await change.locator('select[name=user_id]').selectOption({ label: tech.name });
    await change.locator('input[name=date]').fill(thu);
    await change.locator('input[name=starts]').fill('14:00');
    await change.locator('input[name=ends]').fill('23:00');
    await change.locator('button[value=shift]').click();
    await expect(o).toHaveURL(new RegExp(`/rota\\?week=${mon}`));
    const erik = o.locator('table.rota tr', { hasText: tech.name });
    await expect(erik).toContainText('14:00–23:00');
    await expect(o.locator('tr.rota-warn')).toContainText("Only 8.0 hours' rest before this shift; the law asks for 11");
    await o.screenshot({ path: test.info().outputPath('rota.png'), fullPage: true });

    // Erik is told, and reads it.
    const t = await as(techCtx, tech);
    await t.goto('/mine');
    await expect(t.locator('#live-mine')).toContainText('Your schedule has changed');
    await t.goto(`/schedule?week=${mon}`);
    await expect(t.locator('#live-schedule')).toContainText('14:00–23:00');
    await t.setViewportSize({ width: 390, height: 844 });
    await t.screenshot({ path: test.info().outputPath('schedule.png'), fullPage: true });
    await t.goto('/mine');
    await expect(t.locator('#live-mine')).not.toContainText('Your schedule has changed');

    // Off sick on Wednesday: the counter reads that he is away, not why.
    const away = o.locator('form[action="/rota"]:has(button[value=away])');
    await away.locator('select[name=user_id]').selectOption({ label: tech.name });
    await away.locator('input[name=date]').fill(wed);
    await away.locator('select[name=reason]').selectOption('sick');
    await away.locator('button[value=away]').click();
    const d = await as(deskCtx, desk);
    await d.goto(`/rota?week=${mon}`);
    const row = d.locator('table.rota tr', { hasText: tech.name });
    await expect(row).toContainText('away');
    await expect(row).not.toContainText('off sick');
    await t.goto(`/rota?week=${mon}`);
    await expect(t.locator('table.rota tr', { hasText: tech.name })).toContainText('off sick');
    expect(fri).toBeTruthy();
  } finally {
    await ownerCtx.close();
    await techCtx.close();
    await deskCtx.close();
  }
});
