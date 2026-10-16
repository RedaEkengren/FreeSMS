import { test, expect, Page } from '@playwright/test';
import { desk, tech, signIn, signOut, getApproved } from './helpers';

// A job with several operations: what is done, what waits for a part and what
// is left out are told apart on the page. The technician keeps the list from a
// phone with no price in sight; "ready" is not offered while work is left; an
// omission carries its reason and the customer decision it still needs.

async function addLine(page: Page, text: string, hours: string, price: string) {
  await page.fill('input[form=add-line][name=description]', text);
  await page.fill('input[form=add-line][name=quantity]', hours);
  await page.fill('input[form=add-line][name=unit_price]', price);
  await page.locator('button[form=add-line]').click();
  await expect(page.locator('table.lines-table tbody tr', { hasText: text })).toBeVisible();
}

function step(page: Page, text: string) {
  return page.locator('#work-list li', { hasText: text });
}

test('three operations: one done, one waiting for a part, one left out', async ({ page }) => {
  test.setTimeout(120_000);
  await signIn(page, desk);
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', 'WRK001');
  await page.fill('[name=complaint]', 'Bromsar och avgas');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const job = page.url();
  await addLine(page, 'Byte av bromsskivor', '2', '800');
  await addLine(page, 'Byte av avgasrör', '1', '900');
  await getApproved(page);
  await page.getByRole('button', { name: 'Start work' }).click();

  // The agreed work becomes a list in one click, and a step of its own is added.
  await page.getByRole('button', { name: 'Make a list from the agreed work' }).click();
  await expect(step(page, 'Byte av bromsskivor')).toBeVisible();
  await expect(step(page, 'Byte av avgasrör')).toBeVisible();
  await page.locator('#work-list form[action$="/tasks"] input[name=title]').fill('Provkörning');
  await page.getByRole('button', { name: 'Add to the list' }).click();
  await expect(page.locator('#work-list')).toContainText('3 of 3 left');
  await expect(page.getByRole('button', { name: 'It is ready' })).toHaveCount(0);
  await signOut(page);

  // The technician finishes the brakes and is held up on the exhaust. No price
  // on the page, and no way to leave anything out.
  await signIn(page, tech);
  await page.goto(job);
  await expect(page.locator('#work-list')).not.toContainText('kr');
  await expect(page.locator('#work-list input[name=needs_decision]')).toHaveCount(0);
  await step(page, 'Byte av bromsskivor').getByRole('button', { name: 'Done', exact: true }).click();
  await expect(step(page, 'Byte av bromsskivor')).toContainText('Done');
  await step(page, 'Byte av avgasrör').locator('select[name=blocker]').selectOption('part');
  await step(page, 'Byte av avgasrör').getByRole('button', { name: 'It is held up' }).click();
  await expect(step(page, 'Byte av avgasrör')).toContainText('waiting for a part');
  await expect(step(page, 'Byte av avgasrör')).toContainText(tech.name);
  await signOut(page);

  // The counter sees what holds the car up, leaves the road test out with a
  // reason, and is reminded that the customer has to be told.
  await signIn(page, desk);
  await page.goto(job);
  await expect(page.locator('#work-list')).toContainText('2 of 3 left');
  await expect(page.locator('#work-list')).toContainText('1 blocked');
  await expect(page.getByRole('button', { name: 'It is ready' })).toHaveCount(0);
  const road = step(page, 'Provkörning');
  await road.locator('form:has(input[value=omitted]) input[name=note]').fill('Bilen står kvar tills delen kommit');
  await road.locator('input[name=needs_decision]').check();
  await road.getByRole('button', { name: 'Leave it out' }).click();
  await expect(step(page, 'Provkörning')).toContainText('Left out');
  await expect(step(page, 'Provkörning')).toContainText('Bilen står kvar tills delen kommit');
  await expect(page.locator('#work-list')).toContainText('1 to put to the customer');
  await step(page, 'Provkörning').locator('input[name=note]').first().fill('Kunden ringde, ingen provkörning behövs');
  await step(page, 'Provkörning').getByRole('button', { name: 'Record it' }).click();
  await expect(page.locator('#work-list')).not.toContainText('to put to the customer');
  await signOut(page);

  // The part arrives. Nothing is left, so the car can go back.
  await signIn(page, tech);
  await page.goto(job);
  await step(page, 'Byte av avgasrör').getByRole('button', { name: 'Done', exact: true }).click();
  await expect(page.locator('#work-list')).toContainText('All done');
  await signOut(page);
  await signIn(page, desk);
  await page.goto(job);
  await expect(page.getByRole('button', { name: 'It is ready' })).toBeVisible();
  await signOut(page);
});
