import { test, expect, Page } from '@playwright/test';
import { owner, signIn } from './helpers';

// Capacity on the planner, through its forms: one lift, two cars booked on
// one technician at once, the technician off sick that day and the shop shut
// the day after. The week says each of them, and refuses a booking on the
// shut day.

// A Tuesday at least a week ahead, and the Wednesday after it, as a date
// control takes them.
async function tuesday(page: Page): Promise<[string, string]> {
  return page.evaluate(() => {
    const d = new Date(Date.now() + 7 * 86_400_000);
    while (d.getDay() !== 2) d.setDate(d.getDate() + 1);
    const iso = (x: Date) => new Intl.DateTimeFormat('sv-SE').format(x);
    const next = new Date(d);
    next.setDate(d.getDate() + 1);
    return [iso(d), iso(next)];
  });
}

async function book(page: Page, day: string, at: string, plate: string) {
  await page.goto('/calendar');
  const form = page.locator('form[action="/calendar"][method=post]');
  await form.locator('input[name=date]').fill(day);
  await form.locator('input[name=time]').fill(at);
  await form.locator('input[name=minutes]').fill('60');
  await form.locator('select[name=technician_id]').selectOption({ label: 'Erik Mekaniker' });
  await form.locator('input[name=registration]').fill(plate);
  await form.locator('input[name=what]').fill('Service');
  await form.locator('button[type=submit]').click();
}

async function capacity(page: Page, action: string, fields: Record<string, string>) {
  await page.goto('/calendar');
  const form = page.locator(`form[action="/calendar/capacity"]:has(input[name=action][value=${action}])`).first();
  for (const [name, value] of Object.entries(fields)) {
    const field = form.locator(`[name=${name}]`);
    if (await field.evaluate((el) => el.tagName) === 'SELECT') await field.selectOption(value === 'tech' ? { label: 'Erik Mekaniker' } : value);
    else await field.fill(value);
  }
  await form.locator('button[type=submit]').click();
}

test('the planner shows who is away, a shut day and more cars than lifts', async ({ page }) => {
  await signIn(page, owner);
  const [day, after] = await tuesday(page);

  await capacity(page, 'lifts', { lifts: '1' });
  await book(page, day, '09:00', 'CAP 001');
  await book(page, day, '09:30', 'CAP 002');
  await capacity(page, 'away', { user_id: 'tech', date: day, reason: 'holiday' });
  await capacity(page, 'shut', { date: after, reason: 'Inventering' });

  await page.goto(`/calendar?week=${day}`);
  await expect(page.locator('.pl-away')).toHaveCount(1);
  await expect(page.locator('a.pb.pb-short')).toHaveCount(2);
  await expect(page.locator('.pl-note', { hasText: 'Inventering' })).toBeVisible();
  await expect(page.locator('.pl-note', { hasText: '09:30' })).toBeVisible();
  await page.locator('.pl-away').first().scrollIntoViewIfNeeded();
  await page.screenshot({ path: test.info().outputPath('planner.png'), fullPage: true });

  // Booking on the shut day is refused, and says why.
  await book(page, after, '10:00', 'CAP 003');
  await expect(page.getByText('Inventering')).toBeVisible();
  await page.goto(`/calendar?q=CAP003`);
  await expect(page.locator('ul.lines a[href^="/bookings/"]')).toHaveCount(0);
});
