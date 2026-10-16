import { test, expect, Page } from '@playwright/test';
import { desk, signIn } from './helpers';

// A booking is confirmed to the customer before any job exists: previewed,
// confirmed as to who and where, sent through the same queue as everything else
// and read in a real mailbox. A reminder waiting for a booking that is then
// moved is stopped and says so; and what cannot be sent is told by hand.

const customer = { name: 'Britta Bokare', email: 'britta@e2e.test', phone: '070-555 12 34' };

async function aTuesdayWeeksAhead(page: Page): Promise<string> {
  return page.evaluate(() => {
    const d = new Date(Date.now() + 14 * 86_400_000);
    while (d.getDay() !== 2) d.setDate(d.getDate() + 1);
    return new Intl.DateTimeFormat('sv-SE').format(d);
  });
}

test('a booking confirmed by email, a reminder stopped by a move, and told by hand', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page, desk);
  const day = await aTuesdayWeeksAhead(page);
  await page.goto('/calendar');
  const form = page.locator('form[action="/calendar"][method=post]');
  await form.locator('input[name=date]').fill(day);
  await form.locator('input[name=time]').fill('09:00');
  await form.locator('input[name=minutes]').fill('60');
  await form.locator('input[name=registration]').fill('BOK 123');
  await form.locator('input[name=what]').fill('Besiktningsservice');
  await form.locator('input[name=customer_name]').fill(customer.name);
  await form.locator('input[name=customer_phone]').fill(customer.phone);
  await form.locator('button[type=submit]').click();
  await page.goto('/calendar?q=BOK123');
  await page.getByRole('link', { name: /BOK\s?123/ }).first().click();
  await expect(page).toHaveURL(/\/bookings\/[0-9a-f-]{36}$/);
  const booking = page.url();

  // Previewed with the time as the workshop's clock shows it, then queued.
  const tell = page.locator('form[action$="/message"]');
  await tell.locator('select[name=kind]').selectOption('confirm');
  await tell.locator('select[name=channel]').selectOption('email');
  await tell.locator('input[name=address]').fill(customer.email);
  await tell.locator('input[name=arrival]').fill('Kör in på gården och ring på klockan.');
  await tell.getByRole('button', { name: 'See what it says' }).click();
  await expect(page.getByText('This is what will be sent:')).toBeVisible();
  await expect(page.getByText(`${day} 09:00`)).toBeVisible();
  await expect(page.getByText('Kör in på gården och ring på klockan.')).toBeVisible();
  await page.locator('input[name=confirm]').check();
  await page.getByRole('button', { name: 'Queue it' }).click();
  await expect(page).toHaveURL(booking);

  // It arrives in a real mailbox, with the time and the way to reach a person.
  let text = '';
  await expect.poll(async () => {
    const list = await (await page.request.get('http://mailpit:8025/api/v1/messages')).json();
    const m = list.messages.find((x: any) => x.To.some((to: any) => to.Address === customer.email) && /booking/i.test(x.Subject));
    if (!m) return '';
    text = (await (await page.request.get(`http://mailpit:8025/api/v1/message/${m.ID}`)).json()).Text;
    return text;
  }, { timeout: 20_000 }).toContain(`${day} 09:00`);
  expect(text).toContain('Replies to this are not read.');

  // A reminder for the day before waits; the booking then moves, and it is stopped.
  const again = page.locator('form[action$="/message"]');
  await again.locator('select[name=kind]').selectOption('reminder');
  await again.locator('select[name=channel]').selectOption('email');
  await again.locator('input[name=address]').fill(customer.email);
  await again.getByRole('button', { name: 'See what it says' }).click();
  await page.locator('input[name=confirm]').check();
  await page.getByRole('button', { name: 'Queue it' }).click();
  await page.locator('form:has(input[value=move])').locator('input[name=time]').fill('13:00');
  await page.locator('form:has(input[value=move])').getByRole('button', { name: 'Move it' }).click();
  await expect(page.locator('ul.lines', { hasText: 'moved before this went' }).first()).toBeVisible();

  // And when nothing could be sent it is told by hand.
  await page.locator('form:has(input[name=by_hand])').locator('input[name=how]').fill('Ringde, hon bekräftade');
  await page.locator('form:has(input[name=by_hand])').getByRole('button', { name: 'Write it down' }).click();
  await expect(page.getByText('Ringde, hon bekräftade')).toBeVisible();
});
