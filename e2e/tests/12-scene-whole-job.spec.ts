import { test, expect, Page, BrowserContext } from '@playwright/test';
import { owner, tech, desk, signIn, leave } from './helpers';

// The site's main scene, one job from start to finish, performed against the
// real product in Swedish. Every role in its own browser, the customer in one
// with nobody signed in: taken in, inspected and photographed, approved on
// the customer's link, priced; a part that is not on the shelf, the car home
// while it is ordered, the part arriving and booked in by the parts desk,
// the car back, the part taken out to the job by its code, a final check,
// ready, the customer told and shown it on their link, invoiced, paid in
// cash, collected -- and closed by itself -- and the owner's figures.
//
// site/src/index.html links this test, and the scene shows these steps.

const parts = { name: 'Lisa Lager', email: 'parts@e2e.test', password: 'lagrets lösenfras 1' };
const customer = { name: 'Hela Dagen', phone: '070-246 80 12' };
const partCode = '7391234567890';
const pixel = Buffer.from(
  '/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/wAALCAABAAEBAREA/8QAFAABAAAAAAAAAAAAAAAAAAAACf/EABQQAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQEAAD8AVN//2Q==',
  'base64');

// The day it is, in the shop's calendar, plus some days: what a date control
// takes.
async function shopDay(page: Page, days: number): Promise<string> {
  return page.evaluate((d) => new Intl.DateTimeFormat('sv-SE', { timeZone: 'Europe/Stockholm' })
    .format(new Date(Date.now() + d * 86_400_000)), days);
}

async function as(context: BrowserContext, who: { email: string; password: string }): Promise<Page> {
  const page = await context.newPage();
  await signIn(page, who);
  return page;
}

test.describe.serial('the whole job scene', () => {
  test('one job, start to finish, through every role', async ({ browser }) => {
    test.setTimeout(120_000);
    const ownerCtx = await browser.newContext();
    const o = await as(ownerCtx, owner);

    // The workshop, as the scene's is: a parts desk, a final check, a part
    // with a barcode, and Swedish.
    await o.goto('/staff');
    const add = o.locator('form[action="/staff"]');
    await add.locator('input[name=name]').fill(parts.name);
    await add.locator('input[name=email]').fill(parts.email);
    await add.locator('select[name=role]').selectOption('parts');
    await add.locator('input[name=password]').fill(parts.password);
    await add.locator('button[type=submit]').click();
    await expect(o.getByText(parts.name)).toBeVisible();

    await o.goto('/checklists');
    await o.fill('input[name=name]', 'Slutkontroll');
    await o.fill('textarea[name=checkpoints]', 'Provkörd\nInga varningslampor');
    await o.locator('form[action="/checklists"] button[type=submit]').click();
    await o.goto('/shop');
    await o.locator('form[action="/shop/final-check"] select[name=template_id]').selectOption({ label: 'Slutkontroll' });
    await o.locator('form[action="/shop/final-check"] button[type=submit]').click();

    await o.goto('/stock/parts/new');
    await o.fill('input[name=number]', 'BP-200');
    await o.fill('input[name=name]', 'Bromsbelägg bak');
    await o.fill('input[name=price]', '845');
    await o.fill('textarea[name=codes]', partCode);
    await o.locator('form[action="/stock/parts"] button[type=submit]').click();
    await expect(o.locator('h1')).toContainText('BP-200');

    await o.goto('/shop');
    const details = o.locator('form[action="/shop"]');
    await details.locator('select[name=locale]').selectOption('sv');
    await details.locator('button[type=submit]').click();

    const deskCtx = await browser.newContext();
    const techCtx = await browser.newContext();
    const partsCtx = await browser.newContext();
    const phoneCtx = await browser.newContext();
    try {
      const d = await as(deskCtx, desk);
      const t = await as(techCtx, tech);
      const p = await as(partsCtx, parts);
      const phone = await phoneCtx.newPage();

      // 07:30 -- the car is left at the counter.
      await d.goto('/jobs/new');
      await d.getByLabel('Registreringsnummer').fill('JOB777');
      await d.getByLabel('Vad är fel').fill('Gnisslar vid inbromsning');
      await d.getByRole('button', { name: 'Öppna jobbet' }).click();
      await expect(d).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
      const job = d.url();
      await d.getByRole('link', { name: 'Ange vem som betalar' }).click();
      await d.fill('form[action$="/customer"] input[name=name]', customer.name);
      await d.fill('form[action$="/customer"] input[name=phone]', customer.phone);
      await d.getByRole('button', { name: 'Spara och använd' }).click();
      await d.fill('input[form=add-line][name=description]', 'Felsökning bromsar');
      await d.fill('input[form=add-line][name=quantity]', '1');
      await d.fill('input[form=add-line][name=unit_price]', '895');
      await d.locator('button[form=add-line]').click();
      for (const action of ['Prissätt den', 'Fråga kunden', 'Markera som godkänd']) {
        await d.getByRole('button', { name: action }).click();
      }

      // The technician starts, inspects and photographs.
      await t.goto(job);
      await t.getByRole('button', { name: 'Börja jobba' }).click();
      await t.locator('form[action$="/inspect"] select[name=template_id]').selectOption({ label: 'Vårservice' });
      await t.getByRole('button', { name: 'Starta', exact: true }).click();
      const inspection = t.url();
      const brakes = t.locator('.card', { hasText: 'Bromsar fram' });
      await brakes.locator('input[name=note]').fill('Bakre beläggen nere på 2 mm');
      await brakes.getByRole('button', { name: 'Underkänd' }).click();
      const photo = t.locator('.card', { hasText: 'Bromsar fram' }).locator('form[action$="/photo"]');
      await photo.locator('input[type=file]').setInputFiles({ name: 'pads.jpg', mimeType: 'image/jpeg', buffer: pixel });
      await photo.getByRole('button', { name: 'Lägg till fotot' }).click();
      await t.getByRole('button', { name: 'Avsluta kontrollen' }).click();

      // The customer says yes on their phone.
      await d.goto(inspection);
      await d.getByRole('button', { name: 'Gör en länk till kunden' }).click();
      const inspectionLink = new URL(await d.locator('#share-url').inputValue());
      await phone.goto(new URL(inspectionLink.pathname, d.url()).toString());
      await phone.locator('.card', { hasText: 'Bromsar fram' }).getByRole('button', { name: 'Ja, gör det' }).click();
      await expect(phone.getByText('Du sa ja till det här. Du kan ändra dig.')).toBeVisible();

      // The yes reaches the counter and becomes a line.
      await d.goto('/board');
      await expect(d.locator('a.card', { hasText: /JOB\s?777/ })).toContainText('1 sak godkänd av kunden, inte prissatt');
      await d.goto(job);
      const answer = d.locator('li', { hasText: 'Bromsar fram: Bakre beläggen nere på 2 mm' }).filter({ has: d.locator('form') });
      await answer.locator('input[name=unit_price]').fill('1290');
      await answer.getByRole('button', { name: 'Gör det till en rad' }).click();

      // The part is not on the shelf: asked for, and the job waits.
      await t.goto(job);
      await t.locator('form[action$="/parts"] input[name=description]').fill('Bromsbelägg bak, BP-200');
      await t.getByRole('button', { name: 'Fråga lagret' }).click();
      // Asking for it is what parks the job.
      await expect(t.getByText('Väntar på delar').first()).toBeVisible();

      // The car goes home until Thursday.
      await d.goto(job);
      await d.locator('form[action$="/car"] input[name=expected_back]').fill(await shopDay(d, 2));
      await d.getByRole('button', { name: 'Kunden har hämtat bilen' }).click();
      await d.goto('/board');
      await expect(d.locator('a.card', { hasText: /JOB\s?777/ })).toContainText('Bilen är hemma');

      // The parts desk: the part arrives and is booked in by its barcode.
      await p.goto('/parts');
      await p.locator('.card', { hasText: 'Bromsbelägg bak, BP-200' }).getByRole('button', { name: 'Den har kommit' }).click();
      await p.goto('/scan?code=' + partCode);
      const book = p.locator('form[action="/stock/move"]');
      await book.locator('input[name=quantity]').fill('1');
      await book.locator('select[name=kind]').selectOption('received');
      await book.locator('button[type=submit]').click();

      // The car is back, and the technician takes the part out by its code.
      await d.goto(job);
      await d.getByRole('button', { name: 'Bilen är tillbaka' }).click();
      // The part arriving is what put the job back to work.
      await t.goto(job);
      await expect(t.getByText('Under arbete').first()).toBeVisible();
      await t.locator('form[action$="/takeout"] input[name=code]').fill(partCode);
      await t.getByRole('button', { name: 'Ta ut till jobbet' }).click();
      await expect(t.locator('li', { hasText: 'BP-200 · Bromsbelägg bak' })).toContainText('uttagen, inte prissatt');

      // The parts desk sees it go.
      await p.goto('/scan?code=' + partCode);
      await expect(p.getByText('uttagen till ett jobb').first()).toBeVisible();

      // The counter prices the part the technician took.
      await d.goto(job);
      const swapped = d.waitForResponse((r) => r.url().includes('/parts?q=') && r.ok());
      await d.fill('input[name=q]', 'BP-200');
      await swapped;
      await d.locator('#shelf form.pick', { hasText: 'BP-200' }).locator('button[type=submit]').first().click();
      await expect(d.locator('li', { hasText: 'BP-200 · Bromsbelägg bak' })).toContainText('uttagen');

      // The final check, and ready.
      await t.goto(job);
      await expect(t.getByRole('button', { name: 'Den är klar' })).toHaveCount(0);
      await t.locator('form[action$="/inspect"] select[name=template_id]').selectOption({ label: 'Slutkontroll' });
      await t.getByRole('button', { name: 'Starta', exact: true }).click();
      for (const label of ['Provkörd', 'Inga varningslampor']) {
        await t.locator('.card', { hasText: label }).getByRole('button', { name: 'OK', exact: true }).click();
      }
      await t.getByRole('button', { name: 'Avsluta kontrollen' }).click();
      await t.goto(job);
      await t.getByRole('button', { name: 'Den är klar' }).click();

      // The counter sees it, tells the customer, and the customer sees it.
      await d.goto('/board');
      const card = d.locator('a.card', { hasText: /JOB\s?777/ });
      await expect(card).toContainText('kunden inte meddelad');
      await expect(card).toContainText(`slutkontroll: ${tech.name}`);
      await d.goto(job);
      await d.getByRole('button', { name: 'Gör en länk till kunden' }).click();
      const jobLink = new URL(await d.locator('#share-url').inputValue());
      await d.locator('form[action$="/told"] select[name=how]').selectOption('link');
      await d.locator('form[action$="/told"] button[type=submit]').click();
      await d.goto('/board');
      await expect(d.locator('a.card', { hasText: /JOB\s?777/ })).toContainText('kunden meddelad');
      await phone.goto(new URL(jobLink.pathname, d.url()).toString());
      await expect(phone.getByText('Din bil är klar att hämta.')).toBeVisible();

      // Collected and paid at the counter: the job closes itself.
      await d.goto(job);
      await d.getByRole('button', { name: 'Fakturera den' }).click();
      await d.getByRole('link', { name: /A-\d+/ }).first().click();
      const pay = d.locator('form[action$="/payments"]');
      await pay.locator('select[name=method]').selectOption('cash');
      await pay.locator('button[type=submit]').click();
      await d.goto(job);
      await d.getByRole('button', { name: 'Kunden har hämtat bilen' }).click();
      await expect(d.getByText('Avslutad').first()).toBeVisible();
      await phone.reload();
      await expect(phone.getByText('Det här jobbet är avslutat.')).toBeVisible();

      // The owner reads the result.
      await o.goto('/figures');
      await expect(o.getByRole('heading', { name: 'Siffror' })).toBeVisible();
      await expect(o.getByText('Utfärdade fakturor')).toBeVisible();
    } finally {
      await o.goto('/shop');
      await o.locator('form[action="/shop/final-check"] select[name=template_id]').selectOption('');
      await o.locator('form[action="/shop/final-check"] button[type=submit]').click();
      const details = o.locator('form[action="/shop"]');
      await details.locator('select[name=locale]').selectOption('en');
      await details.locator('button[type=submit]').click();
      await leave(o);
      for (const c of [ownerCtx, deskCtx, techCtx, partsCtx, phoneCtx]) await c.close();
    }
  });
});
