import { test, expect, Page } from '@playwright/test';
import { desk, tech, signIn, signOut, getApproved } from './helpers';

// A colleague taking over a job reads one list: what happened, what was agreed
// and what is left. The technician leaves a note for the next shift and
// corrects it; the counter records a call that was made earlier; the
// technician's list never carries the counter's side.

test('a shift handover from one list, with a corrected note and a call written down later', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page, desk);
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', 'TML001');
  await page.fill('[name=complaint]', 'Skrapande ljud fram');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const job = page.url();
  await page.fill('input[form=add-line][name=description]', 'Byte av bromsskivor');
  await page.fill('input[form=add-line][name=quantity]', '2');
  await page.fill('input[form=add-line][name=unit_price]', '800');
  await page.locator('button[form=add-line]').click();
  await expect(page.locator('table.lines-table tbody tr', { hasText: 'Byte av bromsskivor' })).toBeVisible();
  await getApproved(page);
  await page.getByRole('button', { name: 'Start work' }).click();
  await page.getByRole('button', { name: 'Make a list from the agreed work' }).click();
  await signOut(page);

  // The technician leaves a note for the next shift and then corrects it.
  await signIn(page, tech);
  await page.goto(`${job}/timeline`);
  await expect(page.getByRole('heading', { name: 'What has happened' })).toBeVisible();
  await page.fill('form[action$="/notes"] textarea[name=body]', 'Vänster skiva har fastrostad bult');
  await page.getByRole('button', { name: 'Save the note' }).click();
  await expect(page.locator('ul.lines', { hasText: 'Vänster skiva har fastrostad bult' })).toBeVisible();
  await page.locator('details', { hasText: 'Correct it' }).first().locator('summary').click();
  await page.locator('details textarea[name=body]').fill('Höger skiva har fastrostad bult');
  await page.getByRole('button', { name: 'Save the correction' }).click();
  await expect(page.getByText('Höger skiva har fastrostad bult').first()).toBeVisible();
  await expect(page.getByText('corrected', { exact: true })).toBeVisible();
  await expect(page.getByText('Where it stands')).toBeVisible();
  await expect(page.locator('main')).not.toContainText('Quote version');
  await expect(page.getByText('Record a conversation with the customer')).toHaveCount(0);
  await signOut(page);

  // The counter writes down a call made an hour ago, which is not an approval.
  await signIn(page, desk);
  await page.goto(`${job}/timeline`);
  await expect(page.getByText('Höger skiva har fastrostad bult').first()).toBeVisible();
  await expect(page.getByText('Quote version 1 put to the customer')).toBeVisible();
  const hourAgo = new Date(Date.now() - 3600_000 - new Date().getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
  await page.fill('form:has(input[value=conversation]) textarea[name=body]', 'Kunden ringde och vill ha bilen på fredag');
  await page.fill('form:has(input[value=conversation]) input[name=occurred]', hourAgo);
  await page.getByRole('button', { name: 'Save the conversation' }).click();
  await expect(page.locator('ul.lines', { hasText: 'Kunden ringde och vill ha bilen på fredag' })).toContainText('written down');
  await signOut(page);

  // The next technician does not see the call.
  await signIn(page, tech);
  await page.goto(`${job}/timeline`);
  await expect(page.getByText('Kunden ringde')).toHaveCount(0);
  await expect(page.getByText('Byte av bromsskivor').first()).toBeVisible();
  await signOut(page);
});
