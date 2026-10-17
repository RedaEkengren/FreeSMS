import { test, expect, Page } from '@playwright/test';
import AxeBuilder from '@axe-core/playwright';
import { owner, desk, tech, signIn, signOut, pricedJob } from './helpers';

// Everyday work on a counter computer and on a technician's phone, in Swedish
// and in the English fallback: no automatic accessibility violations, a
// keyboard that gets past the navigation and always sees where it is, no
// sideways scrolling and controls big enough to hit on a phone, a status that
// is never only a colour, and a refusal that is announced in the reader's
// language.

const rules = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'];

async function inLanguage(page: Page, locale: string) {
  await page.goto('/account');
  await page.selectOption('select[name=locale]', locale);
  await page.locator('form[action="/account/language"] button').click();
}

async function clean(page: Page, paths: string[]) {
  const found: string[] = [];
  for (const p of paths) {
    await page.goto(p);
    const r = await new AxeBuilder({ page }).withTags(rules).analyze();
    for (const v of r.violations) {
      found.push(`${p}: ${v.id} (${v.impact}) ${v.nodes[0]?.html.slice(0, 100)}`);
    }
  }
  expect(found, found.join('\n')).toEqual([]);
}

test('the everyday pages have no automatic accessibility violations, in Swedish and in English', async ({ page }) => {
  test.setTimeout(240_000);
  await signIn(page, desk);
  const job = await pricedJob(page, 'TILL01');
  const jobPaths = ['/', '/board', '/jobs/new', job.replace(/^https?:\/\/[^/]+/, ''), '/mine', '/parts', '/stock', '/scan', '/customers',
    '/calendar', '/figures', '/day', '/accounting', '/receivables', '/enquiries', '/search', '/time', '/rota', '/labour', '/help'];
  for (const locale of ['', 'sv']) {
    await inLanguage(page, locale);
    try {
      await clean(page, jobPaths);
    } finally {
      await inLanguage(page, '');
    }
  }

  // The dark theme is the same pages again, and its colours are checked for contrast too.
  await page.goto('/account');
  await page.selectOption('select[name=theme]', 'dark');
  await page.locator('form[action="/account/theme"] button').click();
  try {
    await clean(page, ['/', '/board', '/jobs/new', job.replace(/^https?:\/\/[^/]+/, ''), '/mine', '/stock', '/customers']);
  } finally {
    await page.goto('/account');
    await page.selectOption('select[name=theme]', '');
    await page.locator('form[action="/account/theme"] button').click();
  }
  await signOut(page);

  await signIn(page, owner);
  for (const locale of ['', 'sv']) {
    await inLanguage(page, locale);
    try {
      await clean(page, ['/shop', '/staff', '/import', '/start', '/start/invoice', '/privacy', '/outsourced', '/help/owner']);
    } finally {
      await inLanguage(page, '');
    }
  }
  await signOut(page);

  await signIn(page, tech);
  await clean(page, ['/', '/mine', '/time', '/help/workshop']);
  await signOut(page);
});

test('a keyboard gets past the navigation and always sees where it is', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page, desk);
  const job = await pricedJob(page, 'TANG01');
  await page.goto(job);

  // The first stop is the way past the navigation, and it is on the screen when it has the focus.
  await page.keyboard.press('Tab');
  const skip = page.locator('a.skip');
  await expect(skip).toBeFocused();
  const box = await skip.boundingBox();
  expect(box && box.y >= 0 && box.x >= 0).toBeTruthy();
  await page.keyboard.press('Enter');
  expect(await page.evaluate(() => document.activeElement?.id)).toBe('main');

  // Every stop shows that it has the focus: an outline or a shadow, not just a colour.
  const invisible: string[] = [];
  for (let i = 0; i < 60; i++) {
    await page.keyboard.press('Tab');
    const seen = await page.evaluate(() => {
      const el = document.activeElement as HTMLElement | null;
      if (!el || el === document.body) return null;
      const s = getComputedStyle(el);
      const outlined = s.outlineStyle !== 'none' && parseFloat(s.outlineWidth) > 0;
      const shadowed = s.boxShadow !== 'none';
      return { tag: el.tagName + (el.getAttribute('name') ? '[' + el.getAttribute('name') + ']' : '') + ' ' + s.outlineStyle + '/' + s.outlineWidth + ' fv=' + el.matches(':focus-visible'), shown: outlined || shadowed };
    });
    if (seen && !seen.shown) invisible.push(seen.tag);
  }
  expect(invisible, 'stops with no visible focus: ' + invisible.join(', ')).toEqual([]);
  await signOut(page);
});

test('the primary screens fit a phone, with controls to hit and status in words', async ({ browser }) => {
  test.setTimeout(150_000);
  const ctx = await browser.newContext({ viewport: { width: 360, height: 640 }, hasTouch: true, isMobile: true });
  const page = await ctx.newPage();
  await signIn(page, desk);
  const job = await pricedJob(page, 'TELE01');
  const path = job.replace(/^https?:\/\/[^/]+/, '');
  const problems: string[] = [];
  for (const locale of ['', 'sv']) {
    await inLanguage(page, locale);
    for (const p of ['/', '/jobs/new', path, '/mine', '/parts', '/stock', '/customers']) {
      await page.goto(p);
      const r = await page.evaluate(() => {
        const out = { wide: document.documentElement.scrollWidth > window.innerWidth + 1, small: [] as string[], bare: [] as string[] };
        const visible = (el: Element) => {
          const b = (el as HTMLElement).getBoundingClientRect();
          const s = getComputedStyle(el);
          return b.width > 0 && b.height > 0 && s.visibility !== 'hidden' && s.display !== 'none';
        };
        document.querySelectorAll('button:not(.link), input:not([type=hidden]):not([type=checkbox]):not([type=radio]):not([type=file]), select').forEach((el) => {
          if (!visible(el)) return;
          const b = (el as HTMLElement).getBoundingClientRect();
          if (b.height < 40) out.small.push(el.tagName + (el.getAttribute('name') ? '[' + el.getAttribute('name') + ']' : '') + ' ' + Math.round(b.height) + 'px');
        });
        document.querySelectorAll('.tag').forEach((el) => {
          if (visible(el) && !(el.textContent || '').trim()) out.bare.push(el.className);
        });
        return out;
      });
      if (r.wide) problems.push(`${locale || 'en'} ${p}: scrolls sideways`);
      if (r.small.length) problems.push(`${locale || 'en'} ${p}: small controls ${r.small.slice(0, 4).join(', ')}`);
      if (r.bare.length) problems.push(`${locale || 'en'} ${p}: status with no words ${r.bare.join(', ')}`);
    }
    await inLanguage(page, '');
  }
  expect(problems, problems.join('\n')).toEqual([]);
  await ctx.close();
});

test('a refusal is announced, and says what is wrong in the language of the reader', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page, owner);
  try {
    await inLanguage(page, 'sv');
    await page.goto('/import');
    await page.locator('select[name=kind]').selectOption('parts');
    await page.locator('input[name=file]').setInputFiles({ name: 'fel.csv', mimeType: 'text/csv', buffer: Buffer.from('kolumn;annan\n1;2\n', 'utf-8') });
    await page.locator('form[action="/import"] button[type=submit]').click();
    const alert = page.getByRole('alert');
    await expect(alert).toBeVisible();
    await expect(alert).toContainText('filen har ingen kolumn');
    await expect(page.getByRole('heading', { name: 'Filen kan inte importeras' })).toBeVisible();
    await expect(page.locator('html')).toHaveAttribute('lang', 'sv');
  } finally {
    await inLanguage(page, '');
  }
  await signOut(page);
});

// The customer card has a section that is shut by default -- erasing a person --
// and a closed <details> is skipped by the scan, so it is opened first.
test('the customer card, with its personal-data section open, has no violations in either language', async ({ page }) => {
  await signIn(page, desk);
  await page.goto('/customers');
  // Not every card has the section: a company has no person to erase, and a person already erased has nothing left to.
  const links = await page.locator('a[href^="/customers/"]').evaluateAll(els => els.map(e => e.getAttribute('href') as string));
  let card = '';
  for (const href of [...new Set(links)]) {
    await page.goto(href);
    if (await page.locator('form[action$="/erase"]').count() === 1) { card = href; break; }
  }
  expect(card, 'no customer card with an erase form').not.toBe('');
  const found: string[] = [];
  for (const locale of ['', 'sv']) {
    await inLanguage(page, locale);
    try {
      await page.goto(card);
      await expect(page.locator('form[action$="/erase"]')).toHaveCount(1);
      await page.locator('details').evaluateAll(els => els.forEach(e => ((e as HTMLDetailsElement).open = true)));
      const r = await new AxeBuilder({ page }).withTags(rules).analyze();
      for (const v of r.violations) found.push(`${locale || 'en'}: ${v.id} (${v.impact}) ${v.nodes[0]?.html.slice(0, 100)}`);
    } finally {
      await inLanguage(page, '');
    }
  }
  expect(found, found.join('\n')).toEqual([]);
  await signOut(page);
});
