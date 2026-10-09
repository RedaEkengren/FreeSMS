import { test, expect, Page } from '@playwright/test';
import { tech, signIn } from './helpers';

// Light or dark is the person's choice over the device's, and paper is
// never dark.

const background = (page: Page) => page.evaluate(() => getComputedStyle(document.body).backgroundColor);
const light = 'rgb(244, 245, 247)', dark = 'rgb(14, 17, 22)', white = 'rgb(255, 255, 255)';

async function choose(page: Page, theme: string) {
  await page.goto('/account');
  await page.locator('form[action="/account/theme"] select[name=theme]').selectOption(theme);
  await page.locator('form[action="/account/theme"] button[type=submit]').click();
}

test('the person chooses light or dark over the device, and printing is light', async ({ browser }) => {
  // A light device, a person who chose dark.
  const lightDevice = await browser.newContext({ colorScheme: 'light' });
  const p = await lightDevice.newPage();
  await signIn(p, tech);
  await p.goto('/');
  expect(await background(p)).toBe(light);
  try {
    await choose(p, 'dark');
    await p.goto('/');
    expect(await background(p)).toBe(dark);
    await p.emulateMedia({ media: 'print' });
    expect(await background(p)).toBe(white);
    await p.emulateMedia({ media: 'screen' });

    // A dark device, the same person choosing light.
    const darkDevice = await browser.newContext({ colorScheme: 'dark' });
    const q = await darkDevice.newPage();
    await signIn(q, tech);
    await choose(q, 'light');
    await q.goto('/');
    expect(await background(q)).toBe(light);
    // Nobody signed in follows the device.
    const r = await darkDevice.newPage();
    await darkDevice.clearCookies();
    await r.goto('/login');
    expect(await background(r)).toBe(dark);
    await darkDevice.close();
  } finally {
    await choose(p, '');
    await lightDevice.close();
  }
});
