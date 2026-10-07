import { test, expect } from '@playwright/test';
import { tech, desk, signIn, signOut, leave, readIn, pricedJob } from './helpers';

// The marketing page's "one job, two screens" scene, performed against the
// real product in Swedish. The scene claims the technician's page does not
// hide the customer but never fetches them; the page source is the evidence,
// so it is read, not only what is visible.
test.describe.serial('the two screens scene', () => {
  test('one job opened by the technician and the front desk shows the customer to one of them', async ({ page }) => {
    const name = 'Lena Två Skärmar';
    const phone = '070-111 22 33';

    await signIn(page, desk);
    const job = await pricedJob(page, 'TWO789', 'Byte av bromsbelägg bak');
    await page.getByRole('link', { name: 'Say who is paying' }).click();
    await page.fill('form[action$="/customer"] input[name=name]', name);
    await page.fill('form[action$="/customer"] input[name=phone]', phone);
    await page.getByRole('button', { name: 'Save and use' }).click();
    await expect(page).toHaveURL(job);
    for (const action of ['Price it', 'Ask the customer', 'Mark it approved']) {
      await page.getByRole('button', { name: action }).click();
    }
    await signOut(page);
    await signIn(page, tech);
    await page.goto(job);
    await page.getByRole('button', { name: 'Start work' }).click();
    await signOut(page);
    await signIn(page, desk);

    await readIn(page, 'sv');
    try {
      await page.goto(job);
      await expect(page.getByRole('heading', { name: 'Kund', exact: true })).toBeVisible();
      await expect(page.getByText(name)).toBeVisible();
      await expect(page.getByText(phone)).toBeVisible();
      await expect(page.getByText('Under arbete').first()).toBeVisible();
      await expect(page.getByText('Lova den till')).toBeVisible();
      await expect(page.getByRole('heading', { name: 'Lämna över' })).toHaveCount(0);
    } finally {
      await readIn(page, '');
      await leave(page);
    }

    await signIn(page, tech);
    await readIn(page, 'sv');
    try {
      await page.goto(job);
      await expect(page.getByText('Byte av bromsbelägg bak')).toBeVisible();
      await expect(page.getByRole('heading', { name: 'Lämna över' })).toBeVisible();
      await expect(page.getByText('Jag behöver en del för att komma vidare')).toBeVisible();
      await expect(page.getByRole('heading', { name: 'Kund', exact: true })).toHaveCount(0);
      await expect(page.getByText('Lova den till')).toHaveCount(0);
      // Not hidden: absent from what the server sent.
      const source = await page.content();
      expect(source).not.toContain(name);
      expect(source).not.toContain(phone);
    } finally {
      await readIn(page, '');
      await leave(page);
    }
  });
});
