import { test, expect, Page } from '@playwright/test';
import { desk, signIn, signOut, getApproved } from './helpers';

// A line is entered wrong after the customer has said yes. It is corrected on
// the page with what it comes to shown first; a dearer line needs a new yes
// and the old one is left as it was; a wrong line is withdrawn with a reason
// and stays in the history; and a second person with the page open from
// before is told somebody got there first.

async function addLine(page: Page, text: string, hours: string, price: string) {
  await page.fill('input[form=add-line][name=description]', text);
  await page.fill('input[form=add-line][name=quantity]', hours);
  await page.fill('input[form=add-line][name=unit_price]', price);
  await page.locator('button[form=add-line]').click();
  await expect(page.locator('table.lines-table tbody tr', { hasText: text })).toBeVisible();
}

function row(page: Page, text: string) {
  return page.locator('table.lines-table tbody tr', { hasText: text });
}

test('a line corrected after approval, with what it comes to shown first', async ({ page, browser }) => {
  test.setTimeout(120_000);
  await signIn(page, desk);
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', 'COR001');
  await page.fill('[name=complaint]', 'Skrapande ljud fram');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const job = page.url();
  await addLine(page, 'Byte av bromsklossar', '2', '800');
  await addLine(page, 'Kontrollera bromsvätska', '1', '300');
  await getApproved(page);
  await expect(row(page, 'Byte av bromsklossar')).not.toContainText('added after approval');

  // A wrong price is found: the preview says what it comes to and that the
  // customer has to be asked again, and nothing is saved by looking.
  await row(page, 'Byte av bromsklossar').getByRole('link', { name: 'Correct' }).click();
  await page.fill('input[name=unit_price]', '950');
  await page.getByRole('button', { name: 'See what it comes to' }).click();
  await expect(page.getByRole('heading', { name: 'Check the correction' })).toBeVisible();
  await expect(page.getByText('This line is no longer agreed to.')).toBeVisible();
  await expect(page.getByText('Waiting for the customer:')).toBeVisible();
  await page.goto(job);
  await expect(row(page, 'Byte av bromsklossar')).toContainText('800');
  await expect(row(page, 'Byte av bromsklossar')).not.toContainText('added after approval');

  // Saved, the line goes back to be put to the customer; the other line keeps
  // its yes.
  await row(page, 'Byte av bromsklossar').getByRole('link', { name: 'Correct' }).click();
  await page.fill('input[name=unit_price]', '950');
  await page.getByRole('button', { name: 'See what it comes to' }).click();
  await page.getByRole('button', { name: 'Save the correction' }).click();
  await expect(page).toHaveURL(job);
  await expect(row(page, 'Byte av bromsklossar')).toContainText('950');
  await expect(row(page, 'Byte av bromsklossar')).toContainText('added after approval');
  await expect(row(page, 'Kontrollera bromsvätska')).not.toContainText('added after approval');

  // Fixing the wording of the line that kept its yes keeps it.
  await row(page, 'Kontrollera bromsvätska').getByRole('link', { name: 'Correct' }).click();
  await page.fill('input[name=description]', 'Kontrollera bromsvätskan');
  await page.getByRole('button', { name: 'See what it comes to' }).click();
  await expect(page.getByText("The customer's yes still covers this line")).toBeVisible();
  await page.getByRole('button', { name: 'Save the correction' }).click();
  await expect(row(page, 'Kontrollera bromsvätskan')).not.toContainText('added after approval');

  // A second person has the same line open from before, and is too late.
  const other = await browser.newContext();
  const second = await other.newPage();
  await signIn(second, desk);
  await second.goto(job);
  await row(second, 'Kontrollera bromsvätskan').getByRole('link', { name: 'Correct' }).click();
  await second.fill('input[name=unit_price]', '250');
  await page.goto(job);
  await row(page, 'Kontrollera bromsvätskan').getByRole('link', { name: 'Correct' }).click();
  await page.fill('input[name=unit_price]', '280');
  await page.getByRole('button', { name: 'See what it comes to' }).click();
  await page.getByRole('button', { name: 'Save the correction' }).click();
  await second.getByRole('button', { name: 'See what it comes to' }).click();
  await expect(second.getByText('Somebody else got there first')).toBeVisible();
  await other.close();

  // The wrong line is withdrawn, with a reason, and stays in the history.
  await page.goto(job);
  await row(page, 'Byte av bromsklossar').getByRole('link', { name: 'Correct' }).click();
  await page.fill('form[action$="/withdraw"] input[name=reason]', 'Kunden hade egna klossar');
  await page.getByRole('button', { name: 'See what it changes' }).click();
  await page.getByRole('button', { name: 'Withdraw the line' }).click();
  await expect(row(page, 'Byte av bromsklossar')).toContainText('withdrawn');
  await expect(row(page, 'Byte av bromsklossar')).toContainText('Kunden hade egna klossar');
  await expect(row(page, 'Byte av bromsklossar').getByRole('link', { name: 'Correct' })).toHaveCount(0);
  await signOut(page);
});
