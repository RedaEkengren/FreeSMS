import { test, expect, Page, BrowserContext } from '@playwright/test';
import { tech, desk, signIn } from './helpers';

// Telling the customer from the job page. The car is ready; the front desk
// confirms the address and emails the link; the email arrives -- in a real
// mail server, over SMTP -- and the customer opens the link in it and reads
// that the car is ready. The board says the customer was told. The site's
// scene shows these steps; site/src/index.html links this test.

const customer = { name: 'Karin Kund', email: 'karin@e2e.test' };

async function as(ctx: BrowserContext, who: { email: string; password: string }): Promise<Page> {
  const page = await ctx.newPage();
  await signIn(page, who);
  return page;
}

test('the customer is emailed the link from the job page, and it arrives', async ({ browser }) => {
  const deskCtx = await browser.newContext();
  const techCtx = await browser.newContext();
  const customerCtx = await browser.newContext();
  try {
    const d = await as(deskCtx, desk);
    await d.goto('/jobs/new');
    await d.fill('input[name=registration]', 'MEJ 123');
    await d.fill('[name=complaint]', 'Service');
    await d.getByRole('button', { name: 'Open the job' }).click();
    await expect(d).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
    const job = d.url();
    await d.getByRole('link', { name: 'Say who is paying' }).click();
    await d.fill('form[action$="/customer"] input[name=name]', customer.name);
    await d.fill('form[action$="/customer"] input[name=email]', customer.email);
    await d.locator('form[action$="/customer"] button[type=submit]').click();
    await expect(d).toHaveURL(job);
    await d.fill('input[form=add-line][name=description]', 'Service');
    await d.fill('input[form=add-line][name=unit_price]', '1500');
    await d.locator('button[form=add-line]').click();
    for (const action of ['Price it', 'Ask the customer', 'Mark it approved']) {
      await d.getByRole('button', { name: action }).click();
    }

    const t = await as(techCtx, tech);
    await t.goto(job);
    await t.getByRole('button', { name: 'Start work' }).click();
    await t.getByRole('button', { name: 'It is ready' }).click();

    // The first message to an address is confirmed.
    await d.goto(job);
    const send = d.locator('form[action$="/message"]');
    await expect(send).toBeVisible();
    await send.locator('input[name=confirm_email]').check();
    await send.getByRole('button', { name: 'Email it' }).click();
    await expect(d.locator('#live-telling')).toContainText('sent');

    // It arrived, and says what it is for and how to ring.
    let text = '';
    await expect.poll(async () => {
      const list = await (await d.request.get('http://mailpit:8025/api/v1/messages')).json();
      const m = list.messages.find((x: any) => x.To.some((to: any) => to.Address === customer.email));
      if (!m) return '';
      expect(m.Subject).toBe('Your car MEJ 123');
      text = (await (await d.request.get(`http://mailpit:8025/api/v1/message/${m.ID}`)).json()).Text;
      return text;
    }, { timeout: 15_000 }).toContain('is ready to collect');
    expect(text).toContain('Replies to this are not read.');

    // The customer opens the link in it.
    const link = text.match(/https?:\/\/\S+\/k\/\S+/)![0].replace(/^https?:\/\/[^/]+/, '');
    const c = await customerCtx.newPage();
    await c.goto(link);
    await expect(c.getByText('Your car is ready to collect.')).toBeVisible();

    // And the board says the customer was told.
    await d.goto('/board');
    await expect(d.locator('a.card', { hasText: /MEJ\s?123/ })).toContainText('customer told');
  } finally {
    await deskCtx.close();
    await techCtx.close();
    await customerCtx.close();
  }
});
