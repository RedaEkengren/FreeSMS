import { expect, Page } from '@playwright/test';

// Test values for a disposable workshop. Nothing here is a real person or a
// real password; the database is created empty and thrown away with the run.
export const owner = { name: 'Olle Ägare', email: 'owner@e2e.test', password: 'en lång lösenfras för ägaren' };
export const tech = { name: 'Erik Mekaniker', email: 'tech@e2e.test', password: 'teknikerns lösenfras 1' };
export const desk = { name: 'Disa Disk', email: 'desk@e2e.test', password: 'receptionens lösenfras 1' };

export async function signIn(page: Page, who: { email: string; password: string }) {
  await page.goto('/login');
  await page.fill('input[name=email]', who.email);
  await page.fill('input[name=password]', who.password);
  // By the form, not the button's words: the door speaks the workshop's
  // language, which a scene's test makes Swedish.
  await page.locator('form[action="/login"] button[type=submit]').click();
  await expect(page).not.toHaveURL(/\/login/);
}

export async function signOut(page: Page) {
  await page.getByRole('button', { name: 'Sign out' }).click();
  await expect(page).toHaveURL(/\/login/);
}

// Every alert keep.js shows is accepted, and remembered so a test can say
// which it expected.
export function collectAlerts(page: Page): string[] {
  const seen: string[] = [];
  page.on('dialog', async (d) => {
    seen.push(d.message());
    await d.accept();
  });
  return seen;
}

// A car taken in by the front desk, priced with one labour line; returns the
// job's URL. Everything through the forms.
export async function pricedJob(page: Page, registration: string, line = 'Felsökning'): Promise<string> {
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', registration);
  await page.fill('[name=complaint]', `Kontroll av ${registration}`);
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  await page.fill('input[form=add-line][name=description]', line);
  await page.fill('input[form=add-line][name=quantity]', '1');
  await page.fill('input[form=add-line][name=unit_price]', '500');
  await page.locator('button[form=add-line]').click();
  await expect(page.getByText(line).first()).toBeVisible();
  return page.url();
}

// The lines on a job as the page shows them.
export async function lineCount(page: Page, job: string, text: string): Promise<number> {
  await page.goto(job);
  return page.locator('table.lines-table tbody tr', { hasText: text }).count();
}

// Names a new customer on the job open in the page, through its form.
export async function nameCustomer(page: Page, name: string) {
  const job = page.url();
  await page.getByRole('link', { name: 'Say who is paying' }).click();
  await page.fill('form[action$="/customer"] input[name=name]', name);
  await page.getByRole('button', { name: 'Save and use' }).click();
  await expect(page).toHaveURL(job);
}

// Signing out in whatever language the page is in.
export async function leave(page: Page) {
  await page.locator('form[action="/logout"] button').click();
  await expect(page).toHaveURL(/\/login/);
}

// The language the signed-in person reads; '' is the workshop's.
export async function readIn(page: Page, locale: string) {
  await page.goto('/account');
  await page.selectOption('select[name=locale]', locale);
  await page.locator('form[action="/account/language"] button').click();
}

// A wall clock in the shop's zone, minutes from now, as a datetime-local
// control takes it. The browser here runs in UTC; the shop is in Stockholm,
// and the page writes and reads the shop's clock.
export async function shopClock(page: Page, minutesFromNow: number): Promise<string> {
  return page.evaluate((m) => {
    const parts = new Intl.DateTimeFormat('sv-SE', {
      timeZone: 'Europe/Stockholm', year: 'numeric', month: '2-digit', day: '2-digit',
      hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
    }).formatToParts(new Date(Date.now() + m * 60_000));
    const v = (t: string) => parts.find((p) => p.type === t)!.value;
    return `${v('year')}-${v('month')}-${v('day')}T${v('hour')}:${v('minute')}`;
  }, minutesFromNow);
}
