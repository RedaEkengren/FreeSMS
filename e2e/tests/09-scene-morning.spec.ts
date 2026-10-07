import { test, expect } from '@playwright/test';
import { owner, tech, desk, signIn, signOut, leave, readIn } from './helpers';

// The marketing page's morning scene, performed against the real product in
// Swedish: taken in, worked on without the customer, invoiced, and handed to
// the accountant as a SIE file -- the real file, read back. The scene prints
// the verification the product's accounting code writes for this invoice;
// this checks the export writes the same entries.
test.describe.serial('the morning scene', () => {
  test('a car taken in, worked on without the customer, invoiced and handed to the accountant', async ({ page }) => {
    const customer = 'Morgon Kund';
    const phone = '070-765 43 21';
    let job = '';

    // The scene's workshop adds nothing to an invoice; the suite's has had
    // förbrukningsmaterial and a fee since the charges test.
    await signIn(page, owner);
    await page.goto('/shop');
    const charges = page.locator('form[action="/shop/charges"]');
    await charges.locator('input[name=consumables_percent]').fill('0');
    await charges.locator('input[name=consumables_cap]').fill('');
    await charges.locator('input[name=invoice_fee]').fill('0');
    await charges.getByRole('button', { name: 'Save' }).click();
    await signOut(page);

    await signIn(page, desk);
    await readIn(page, 'sv');
    try {
      await page.goto('/jobs/new');
      await expect(page.getByRole('heading', { name: 'Ta in en bil' })).toBeVisible();
      await page.getByLabel('Registreringsnummer').fill('MOR123');
      await page.getByLabel('Mätarställning, km').fill('18 500');
      await page.getByLabel('Vad är fel').fill('Skramlar fram vid 80');
      await page.getByRole('button', { name: 'Öppna jobbet' }).click();
      await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
      job = page.url();

      await page.getByRole('link', { name: 'Ange vem som betalar' }).click();
      await page.fill('form[action$="/customer"] input[name=name]', customer);
      await page.fill('form[action$="/customer"] input[name=phone]', phone);
      await page.locator('form[action$="/customer"] button[type=submit]').click();
      await expect(page).toHaveURL(job);
      await page.fill('input[form=add-line][name=description]', 'Byte av bromsskivor fram');
      await page.fill('input[form=add-line][name=quantity]', '1,5');
      await page.fill('input[form=add-line][name=unit_price]', '895');
      await page.locator('button[form=add-line]').click();
      for (const action of ['Prissätt den', 'Fråga kunden', 'Markera som godkänd']) {
        await page.getByRole('button', { name: action }).click();
      }
      await expect(page.getByText('Godkänd, inte påbörjad').first()).toBeVisible();
    } finally {
      await readIn(page, '');
      await leave(page);
    }

    // The technician: the vehicle and the job, never the customer.
    await signIn(page, tech);
    await readIn(page, 'sv');
    try {
      await page.goto(job);
      await expect(page.getByText('Skramlar fram vid 80')).toBeVisible();
      await expect(page.getByText(customer)).toHaveCount(0);
      await expect(page.getByText(phone)).toHaveCount(0);
      await page.getByRole('button', { name: 'Börja jobba' }).click();
      await page.getByRole('button', { name: 'Den är klar' }).click();
      await expect(page.getByText('Klar för hämtning').first()).toBeVisible();
    } finally {
      await readIn(page, '');
      await leave(page);
    }

    // The front desk invoices, and hands the month to the accountant.
    await signIn(page, desk);
    await readIn(page, 'sv');
    try {
      await page.goto(job);
      await page.getByRole('button', { name: 'Fakturera den' }).click();
      const invoice = page.getByRole('link', { name: /A-\d+/ }).first();
      const reference = (await invoice.textContent())!.match(/A-\d+/)![0];
      await invoice.click();
      const doc = page.locator('article');
      await expect(doc.getByText('Faktureras till')).toBeVisible();
      await expect(doc.getByText(customer)).toBeVisible();
      await expect(doc.getByText('1,5 × 895,00 kr')).toBeVisible();
      await expect(doc.locator('dt', { hasText: 'Summa' }).locator('+ dd')).toHaveText(/^1\s678,13\skr$/);

      await page.goto('/accounting');
      const today = await page.evaluate(() => new Intl.DateTimeFormat('sv-SE', { timeZone: 'Europe/Stockholm' }).format(new Date()));
      await page.fill('input[name=from]', today.slice(0, 8) + '01');
      await page.fill('input[name=to]', today);
      const download = page.waitForEvent('download');
      await page.getByRole('button', { name: 'Ladda ner filen' }).click();
      const file = await (await download).createReadStream();
      const chunks: Buffer[] = [];
      for await (const c of file) chunks.push(c as Buffer);
      // CP437; the lines checked here are ASCII.
      const sie = Buffer.concat(chunks).toString('latin1');
      const [series, number] = reference.split('-');
      const ver = sie.split('#VER').find((v) => v.startsWith(` "${series}" "${number}" `));
      expect(ver, `no verification for ${reference} in the file`).toBeTruthy();
      expect(ver).toContain('#TRANS 1510 {} 1678.13');
      expect(ver).toContain('#TRANS 3010 {} -1342.50');
      expect(ver).toContain('#TRANS 2611 {} -335.63');
    } finally {
      await readIn(page, '');
      await leave(page);
    }
  });
});
