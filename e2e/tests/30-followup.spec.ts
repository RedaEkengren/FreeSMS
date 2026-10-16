import { test, expect, Page, BrowserContext } from '@playwright/test';
import { tech, desk, signIn, getApproved } from './helpers';

// What is owed is chased, not just listed: somebody has it, a conversation and a
// promise are written down, a dispute holds the reminder without touching the
// balance, the reminder that does go carries what is owed when it goes, and
// money owed back after a credit is paid back as a refund -- which is not the
// reversal of a mistake.

const customer = { name: 'Folke Fordran', email: 'folke@e2e.test' };

async function as(ctx: BrowserContext, who: { email: string; password: string }): Promise<Page> {
  const page = await ctx.newPage();
  await signIn(page, who);
  return page;
}

test('an unpaid invoice followed up, reminded, then credited and paid back', async ({ browser }) => {
  test.setTimeout(150_000);
  const deskCtx = await browser.newContext();
  const techCtx = await browser.newContext();
  try {
    const d = await as(deskCtx, desk);
    await d.goto('/jobs/new');
    await d.fill('input[name=registration]', 'FOL123');
    await d.fill('[name=complaint]', 'Service');
    await d.getByRole('button', { name: 'Open the job' }).click();
    await expect(d).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
    const job = d.url();
    await d.getByRole('link', { name: 'Say who is paying' }).click();
    await d.fill('form[action$="/customer"] input[name=name]', customer.name);
    await d.fill('form[action$="/customer"] input[name=email]', customer.email);
    await d.locator('form[action$="/customer"] button[type=submit]').click();
    await expect(d).toHaveURL(job);
    for (const [text, price] of [['Service', '1500'], ['Torkarblad', '300']]) {
      await d.fill('input[form=add-line][name=description]', text);
      await d.fill('input[form=add-line][name=unit_price]', price);
      await d.locator('button[form=add-line]').click();
      await expect(d.locator('table.lines-table tbody tr', { hasText: text })).toBeVisible();
    }
    await getApproved(d);
    const t = await as(techCtx, tech);
    await t.goto(job);
    await t.getByRole('button', { name: 'Start work' }).click();
    await t.getByRole('button', { name: 'It is ready' }).click();
    await d.goto(job);
    await d.getByRole('button', { name: 'Invoice it' }).click();
    await d.getByRole('link', { name: /A-\d+/ }).first().click();
    const invoice = d.url();

    // On the list, nobody has it, and nobody knows it was delivered.
    await d.goto('/receivables');
    const row = d.locator('li', { hasText: customer.name }).filter({ has: d.getByRole('link', { name: 'Follow up' }) });
    await expect(row).toContainText('Nobody has it');
    await expect(row).toContainText('never delivered');
    await row.getByRole('link', { name: 'Follow up' }).click();

    // Taken, with a day to look at it again; a conversation and a promise.
    await d.locator('form:has(input[value=take]) select[name=owner]').selectOption('me');
    await d.locator('form:has(input[value=take]) input[name=next]').fill('2030-03-04');
    await d.locator('form:has(input[value=take])').getByRole('button', { name: 'Save' }).click();
    await d.locator('form:has(input[value=contact]) input[name=note]').fill('Rang, ska betala efter lön');
    await d.locator('form:has(input[value=contact])').getByRole('button', { name: 'Write it down' }).click();
    await d.locator('form:has(input[value=promise]) input[name=amount]').fill('500');
    await d.locator('form:has(input[value=promise]) input[name=by]').fill('2030-03-01');
    await d.locator('form:has(input[value=promise])').getByRole('button', { name: 'Write it down' }).click();
    await expect(d.getByText('Rang, ska betala efter lön')).toBeVisible();
    await expect(d.getByText('promised', { exact: true })).toBeVisible();

    // A dispute puts it on hold; no reminder while it stands. Nothing owed changes.
    await d.locator('form:has(input[value=dispute]) input[name=note]').fill('Säger att torkarbladen aldrig byttes');
    await d.locator('form:has(input[value=dispute])').getByRole('button', { name: 'Put it on hold' }).click();
    await expect(d.getByText('On hold: lift the hold to send a reminder.')).toBeVisible();
    await d.locator('form:has(input[value=resolve]) input[name=note]').fill('Bytte gjordes, bild visad');
    await d.locator('form:has(input[value=resolve])').getByRole('button', { name: 'Lift the hold' }).click();

    // The reminder goes with what is owed.
    const rem = d.locator('form[action$="/reminder"]');
    await rem.locator('select[name=channel]').selectOption('email');
    await rem.locator('input[name=address]').fill(customer.email);
    await rem.locator('input[name=confirm]').check();
    await rem.getByRole('button', { name: 'Queue the reminder' }).click();
    let text = '';
    await expect.poll(async () => {
      const list = await (await d.request.get('http://mailpit:8025/api/v1/messages')).json();
      const m = list.messages.find((x: any) => x.To.some((to: any) => to.Address === customer.email) && /Reminder/.test(x.Subject));
      if (!m) return '';
      text = (await (await d.request.get(`http://mailpit:8025/api/v1/message/${m.ID}`)).json()).Text;
      return text;
    }, { timeout: 20_000 }).toContain('/d/');
    expect(text).toMatch(/2[\s ,.]?250/);
    expect(text).not.toContain('{owed}');

    // Paid in full, then part of it is credited: money is owed back, and it is paid back.
    await d.goto(invoice);
    await d.locator('form[action$="/payments"] select[name=method]').selectOption('bank');
    await d.locator('form[action$="/payments"]').getByRole('button', { name: 'Record the payment' }).click();
    await d.getByRole('link', { name: 'Credit part of it' }).click();
    await d.fill('input[name=qty_2]', '1');
    await d.fill('input[name=reason]', 'Torkarblad byttes inte');
    await d.getByRole('button', { name: 'See what it comes to' }).click();
    await d.getByRole('button', { name: 'Issue the credit note' }).click();
    await d.goto('/receivables');
    const back = d.locator('li', { hasText: customer.name }).filter({ has: d.getByRole('link', { name: 'Pay it back' }) });
    await expect(back).toContainText('owed back to the customer');
    await back.getByRole('link', { name: 'Pay it back' }).click();
    await d.locator('form:has(input[value=refund]) select[name=method]').selectOption('bank');
    await d.locator('form:has(input[value=refund])').getByRole('button', { name: 'It has been paid out' }).click();
    await expect(d.getByText('Refund paid out')).toBeVisible();
    await d.goto('/receivables');
    await expect(d.getByRole('link', { name: 'Pay it back' })).toHaveCount(0);
  } finally {
    await deskCtx.close();
    await techCtx.close();
  }
});
