import { test, expect, Page } from '@playwright/test';
import { owner, desk, tech, signIn, signOut, collectAlerts, pricedJob, nameCustomer } from './helpers';

// A JPEG small enough to write out: one white pixel. What the server checks
// is that it is an image and what it stores is its bytes.
const pixel = Buffer.from(
  '/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/wAALCAABAAEBAREA/8QAFAABAAAAAAAAAAAAAAAAAAAACf/EABQQAQAAAAAAAAAAAAAAAAAAAAD/2gAIAQEAAD8AVN//2Q==',
  'base64');

async function startInspection(page: Page): Promise<string> {
  const job = await pricedJob(page, 'INS001');
  await page.getByRole('button', { name: 'Start it' }).click();
  await expect(page).toHaveURL(/\/inspections\//);
  return job;
}

test.describe.serial('forms the queue has to get right', () => {
  test('the owner makes a checklist', async ({ page }) => {
    await signIn(page, owner);
    await page.goto('/checklists');
    await page.fill('input[name=name]', 'Vårservice');
    await page.fill('textarea[name=checkpoints]', 'Bromsar fram\nDäck');
    await page.getByRole('button', { name: 'Make the checklist' }).click();
    await expect(page.getByRole('link', { name: 'Vårservice' })).toBeVisible();
  });

  test('the button pressed with no connection is the answer recorded', async ({ page, context }) => {
    collectAlerts(page);
    await signIn(page, desk);
    await startInspection(page);

    await context.setOffline(true);
    const first = page.locator('form[action*="/items/"][data-offline]').first();
    await first.locator('input[name=note]').fill('Skivorna under minimimått');
    await first.getByRole('button', { name: 'Fail' }).click();
    await expect(page.locator('#held')).toContainText('waiting to be sent');

    await context.setOffline(false);
    await page.evaluate(() => window.dispatchEvent(new Event('online')));
    await expect(page.locator('#held')).toBeHidden();
    await page.reload();
    await expect(page.locator('.tag.late').first()).toHaveText('fail');
  });

  test('Enter in a note records nothing', async ({ page }) => {
    await signIn(page, desk);
    await startInspection(page);
    const second = page.locator('form[action*="/items/"][data-offline]').nth(1);
    await second.locator('input[name=note]').fill('Slitna, bör bytas');
    await second.locator('input[name=note]').press('Enter');
    await page.waitForTimeout(500);
    await page.reload();
    await expect(page.getByText('not checked')).toHaveCount(2);
  });

  test('a photograph needs a connection, and is kept when there is one', async ({ page, context }) => {
    const alerts = collectAlerts(page);
    await signIn(page, desk);
    await startInspection(page);
    const photoForm = page.locator('form[action$="/photo"]').first();
    await photoForm.locator('input[type=file]').setInputFiles({ name: 'disc.jpg', mimeType: 'image/jpeg', buffer: pixel });

    await context.setOffline(true);
    await photoForm.getByRole('button', { name: 'Add the photograph' }).click();
    await expect.poll(() => alerts.join(' ')).toContain('needs a connection');
    await expect(page.locator('#held')).toBeHidden();

    await context.setOffline(false);
    await photoForm.getByRole('button', { name: 'Add the photograph' }).click();
    await expect(page.locator('.photos img').first()).toBeVisible();
  });

  test('a part from a shelf search, added with no connection, is held', async ({ page, context }) => {
    const alerts = collectAlerts(page);
    await signIn(page, owner);
    await page.goto('/stock/parts/new');
    await page.fill('input[name=number]', 'BP-V70');
    await page.fill('input[name=name]', 'Bromsbelägg fram');
    await page.fill('input[name=price]', '609');
    await page.getByRole('button', { name: 'Add the part' }).click();
    await expect(page.locator('h1')).toContainText('BP-V70');
    await signOut(page);

    await signIn(page, desk);
    const job = await pricedJob(page, 'HTX001');
    // The results are swapped in by htmx, after the page loaded. Waited for,
    // and marked: the short list shown on load has the same part in it, and a
    // click on that would test the form the page started with, not one htmx
    // put there.
    await page.evaluate(() => document.querySelectorAll('#shelf form.pick').forEach((f) => f.setAttribute('data-from-load', '')));
    const swapped = page.waitForResponse((r) => r.url().includes('/parts?q=') && r.ok());
    await page.fill('input[name=q]', 'BP-V');
    await swapped;
    const row = page.locator('#shelf form.pick:not([data-from-load])', { hasText: 'BP-V70' });
    await expect(row).toBeVisible();

    await context.setOffline(true);
    await row.getByRole('button', { name: 'Add' }).click();
    await expect.poll(() => alerts.join(' ')).toContain('being held');

    await context.setOffline(false);
    await page.evaluate(() => window.dispatchEvent(new Event('online')));
    await expect(page.locator('#held')).toBeHidden();
    await page.goto(job);
    await expect(page.locator('table.lines-table tbody tr', { hasText: 'Bromsbelägg fram' })).toHaveCount(1);
  });

  test('two invoices for one job at once make one; two jobs at once make two', async ({ page, browser }) => {
    await signIn(page, desk);
    const jobs: string[] = [];
    for (const reg of ['DBL001', 'DBL002', 'DBL003']) {
      const job = await pricedJob(page, reg);
      await nameCustomer(page, `Kund ${reg}`);
      for (const action of ['Price it', 'Ask the customer', 'Mark it approved']) {
        await page.getByRole('button', { name: action }).click();
      }
      jobs.push(job);
    }
    await signOut(page);
    await signIn(page, tech);
    for (const job of jobs) {
      await page.goto(job);
      await page.getByRole('button', { name: 'Start work' }).click();
      await page.getByRole('button', { name: 'It is ready' }).click();
      await expect(page.getByText('Ready for collection')).toBeVisible();
    }
    await signOut(page);
    await signIn(page, desk);

    // Fired together from the page, with its session, the way a double click
    // or two people at two screens would send them.
    const post = (path: string) =>
      page.evaluate((p) => fetch(p, { method: 'POST' }).then(async (r) => r.status + ' ' + r.url + ' ' + ((await r.text()).match(/class="error">([^<]*)/) || [])[1]), path);
    const paths = jobs.map((j) => new URL(j).pathname + '/invoice');
    const statuses = await Promise.all([post(paths[0]), post(paths[0]), post(paths[1]), post(paths[2])]);
    // Each answer is a redirect to the job, or a refusal that says why; never
    // a server error.
    for (const s of statuses) expect(Number(s.slice(0, 3)), s).toBeLessThan(500);

    const numbers: string[] = [];
    for (const job of jobs) {
      await page.goto(job);
      const refs = await page.getByRole('link', { name: /A-\d+/ }).allTextContents();
      expect(refs, `${job} has ${refs.length} invoices`).toHaveLength(1);
      numbers.push(refs[0].trim());
    }
    expect(new Set(numbers).size).toBe(3);
  });
});
