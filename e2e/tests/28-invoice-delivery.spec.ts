import { test, expect, Page, BrowserContext } from '@playwright/test';
import { tech, desk, signIn, getApproved } from './helpers';

// An invoice to the customer: the counter confirms who it is for, it is
// emailed as a link that opens that one document, the customer reads it and
// the counter sees that it was opened. Sending again closes the first link,
// and a print at the counter is a delivery too.

const customer = { name: 'Dagny Debitor', email: 'dagny@e2e.test' };

async function as(ctx: BrowserContext, who: { email: string; password: string }): Promise<Page> {
  const page = await ctx.newPage();
  await signIn(page, who);
  return page;
}

async function lastMail(d: Page, address: string, after: number): Promise<string> {
  let text = '';
  await expect.poll(async () => {
    const list = await (await d.request.get('http://mailpit:8025/api/v1/messages')).json();
    const ms = list.messages.filter((x: any) => x.To.some((to: any) => to.Address === address));
    if (ms.length <= after) return '';
    text = (await (await d.request.get(`http://mailpit:8025/api/v1/message/${ms[0].ID}`)).json()).Text;
    return text;
  }, { timeout: 15_000 }).toContain('/d/');
  return text;
}

test('an invoice emailed as a link, opened by the customer, and sent again', async ({ browser }) => {
  test.setTimeout(120_000);
  const deskCtx = await browser.newContext();
  const techCtx = await browser.newContext();
  const customerCtx = await browser.newContext();
  try {
    const d = await as(deskCtx, desk);
    await d.goto('/jobs/new');
    await d.fill('input[name=registration]', 'FAK 123');
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
    await getApproved(d);
    const t = await as(techCtx, tech);
    await t.goto(job);
    await t.getByRole('button', { name: 'Start work' }).click();
    await t.getByRole('button', { name: 'It is ready' }).click();
    await d.goto(job);
    await d.getByRole('button', { name: 'Invoice it' }).click();
    await d.getByRole('link', { name: /A-\d+/ }).first().click();
    await expect(d.getByText('not delivered yet')).toBeVisible();
    const invoice = d.url();

    // It is for the company's accounts person, not the vehicle's owner; the
    // counter has to say so before anything is sent.
    const send = d.locator('form[action$="/send"]');
    await send.locator('input[name=recipient]').fill('Dagnys ekonomiavdelning');
    await send.locator('select[name=channel]').selectOption('email');
    await send.locator('input[name=address]').fill(customer.email);
    await send.locator('input[name=confirm]').check();
    await send.getByRole('button', { name: 'Send it' }).click();
    const text = await lastMail(d, customer.email, 0);
    expect(text).toContain('invoice');

    // The customer opens the link: that document, and nothing around it.
    const link = text.match(/https?:\/\/\S+\/d\/\S+/)![0].replace(/^https?:\/\/[^/]+/, '');
    const c = await customerCtx.newPage();
    await c.goto(link);
    await expect(c.getByRole('heading', { name: /Invoice A-\d+/ })).toBeVisible();
    await expect(c.getByText(customer.name)).toBeVisible();
    await expect(c.getByText('Where it stands today')).toBeVisible();
    await expect(c.locator('a[href^="/jobs/"], a[href^="/vehicles/"], a[href="/board"]')).toHaveCount(0);

    // The counter sees it was opened, not just sent.
    await d.goto(invoice);
    await expect(d.getByText('opened by the customer')).toBeVisible();

    // Sent again, the first link stops working.
    await d.locator('form[action$="/send"] input[name=confirm]').check();
    await d.locator('form[action$="/send"]').getByRole('button', { name: 'Send it again' }).click();
    await lastMail(d, customer.email, 1);
    await c.goto(link);
    await expect(c.getByText('This link is no longer open')).toBeVisible();

    // And a print at the counter is recorded as a delivery.
    await d.goto(invoice);
    await d.locator('form[action$="/handover"]').getByRole('button', { name: 'Record it' }).click();
    await expect(d.getByText('printed at the counter').first()).toBeVisible();
  } finally {
    await deskCtx.close();
    await techCtx.close();
    await customerCtx.close();
  }
});
