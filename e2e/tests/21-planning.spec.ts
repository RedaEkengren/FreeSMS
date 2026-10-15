import { test, expect, Page } from '@playwright/test';
import { owner, tech, desk, signIn, signOut } from './helpers';

// The morning plan survives a technician ringing in sick, a repair that takes
// two days and a car that waits for a part: the same jobs, handed over and
// moved, with who and why left on the page and the hours already clocked left
// with the person who clocked them.

const second = { name: 'Tina Tekniker', email: 'tina@e2e.test', password: 'tinas lösenfras 12345' };

async function takeIn(page: Page, registration: string, technician?: string): Promise<string> {
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', registration);
  await page.fill('[name=complaint]', `Planering ${registration}`);
  if (technician) await page.locator('select[name=technician_id]').selectOption({ label: technician });
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  return page.url();
}

async function plan(page: Page, job: string, starts: string, ends: string, hours: string, lift = true) {
  await page.goto(job);
  const form = page.locator('form[action$="/plan"]');
  await form.locator('input[name=starts]').fill(starts);
  await form.locator('input[name=ends]').fill(ends);
  await form.locator('input[name=hours]').fill(hours);
  await form.locator('input[name=on_lift]').setChecked(lift);
  await form.getByRole('button', { name: 'Save the plan' }).click();
}

test('a sick technician, a two-day job and a car waiting for a part', async ({ page, browser }) => {
  test.setTimeout(120_000);

  // The owner has a second technician, and one lift.
  await signIn(page, owner);
  await page.goto('/staff');
  const add = page.locator('form[action="/staff"]');
  await add.locator('input[name=name]').fill(second.name);
  await add.locator('input[name=email]').fill(second.email);
  await add.locator('select[name=role]').selectOption('technician');
  await add.locator('input[name=password]').fill(second.password);
  await page.getByRole('button', { name: 'Add them' }).click();
  await expect(page.getByText(second.name)).toBeVisible();
  await page.goto('/calendar');
  await page.locator('form:has(input[name=lifts])').locator('input[name=lifts]').fill('1');
  await page.locator('form:has(input[name=lifts])').getByRole('button', { name: 'Save' }).click();
  await signOut(page);

  // The desk puts a technician on a job and plans a day for it.
  await signIn(page, desk);
  const job = await takeIn(page, 'PLN001', tech.name);
  await plan(page, job, '2030-03-04T08:00', '2030-03-04T16:00', '8');
  await expect(page.locator('#live-plan')).toContainText(tech.name);
  await expect(page.locator('#live-plan')).toContainText('8.0 h left');
  await expect(page.locator('#live-plan')).toContainText('on a lift');
  await signOut(page);

  // They ring in sick.
  await signIn(page, owner);
  await page.goto('/rota');
  const away = page.locator('form[action="/rota"]').filter({ has: page.getByRole('button', { name: 'Mark as away' }) });
  await away.locator('select[name=user_id]').selectOption({ label: tech.name });
  await away.locator('input[name=date]').fill('2030-03-04');
  await away.locator('select[name=reason]').selectOption('sick');
  await away.getByRole('button', { name: 'Mark as away' }).click();
  await signOut(page);

  // The desk is told, on the job and on the board, and hands it over.
  await signIn(page, desk);
  await page.goto(job);
  await expect(page.locator('#live-plan')).toContainText(`${tech.name} is away on 2030-03-04`);
  await page.goto('/board');
  await expect(page.locator('#backlog')).toContainText('is away; it needs somebody else');
  await page.goto(job);
  const handOver = page.locator('form[action$="/staff"]').filter({ has: page.getByRole('button', { name: 'Hand it over' }) });
  await handOver.locator('select[name=user_id]').selectOption({ label: second.name });
  await handOver.locator('input[name=reason]').fill('sjuk');
  await handOver.getByRole('button', { name: 'Hand it over' }).click();
  await expect(page.locator('#live-plan')).toContainText(second.name);
  await expect(page.locator('#live-plan')).not.toContainText('is away on');
  await page.locator('#live-plan summary', { hasText: 'Who has been on it' }).click();
  await expect(page.locator('#live-plan details').first()).toContainText(tech.name);
  await expect(page.locator('#live-plan details').first()).toContainText('sjuk');
  await page.goto('/board');
  await expect(page.locator('#backlog')).not.toContainText('is away; it needs somebody else');

  // The new technician finds it in their own queue.
  const techCtx = await browser.newContext();
  const tina = await techCtx.newPage();
  try {
    await signIn(tina, second);
    await tina.goto('/mine');
    await expect(tina.locator('#live-mine li', { hasText: 'PLN001' }).first()).toContainText('Put on you');
  } finally {
    await techCtx.close();
  }

  // A job that took two days: the plan runs out with the job still open and
  // is carried over, not booked again.
  await page.goto('/board');
  const cars = await page.locator('a.card').count();
  await plan(page, job, '2020-01-06T08:00', '2020-01-07T12:00', '12');
  await page.goto('/board');
  await expect(page.locator('#backlog')).toContainText('The plan ran out and the job is still open');
  await page.locator('#backlog').getByRole('button', { name: 'Carry it over to the next open day' }).click();
  await expect(page).toHaveURL(job);
  await page.goto('/board');
  await expect(page.locator('#backlog')).not.toContainText('The plan ran out and the job is still open');
  await expect(page.locator('a.card')).toHaveCount(cars);
  await page.goto(job);
  await page.locator('#live-plan summary', { hasText: 'Earlier plans' }).click();
  await expect(page.locator('#live-plan details').last()).toContainText('7 Jan');

  // A car waits for a part and gives up its lift to the next one.
  const waiting = await takeIn(page, 'PLN002');
  await plan(page, waiting, '2030-03-05T08:00', '2030-03-05T16:00', '4');
  const other = await takeIn(page, 'PLN003');
  await plan(page, other, '2030-03-05T08:00', '2030-03-05T16:00', '4');
  await expect(page.getByRole('heading', { name: 'This plan clashes' })).toBeVisible();
  await expect(page.getByText('More cars than lifts')).toBeVisible();
  await plan(page, waiting, '2030-03-05T08:00', '2030-03-05T16:00', '4', false);
  await expect(page.locator('#live-plan')).toContainText('not on a lift');
  await plan(page, other, '2030-03-05T08:00', '2030-03-05T16:00', '4');
  await expect(page.locator('#live-plan')).toContainText('on a lift');
  await expect(page.getByRole('heading', { name: 'This plan clashes' })).toHaveCount(0);

  // And the week shows where the work is.
  await page.goto('/calendar?week=2030-03-04');
  await expect(page.locator('.pb', { hasText: 'PLN002' }).first()).toBeVisible();
  await signOut(page);

  await signIn(page, owner);
  await page.goto('/calendar');
  await page.locator('form:has(input[name=lifts])').locator('input[name=lifts]').fill('0');
  await page.locator('form:has(input[name=lifts])').getByRole('button', { name: 'Save' }).click();
});
