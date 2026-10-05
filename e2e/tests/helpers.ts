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
  await page.getByRole('button', { name: 'Sign in' }).click();
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
