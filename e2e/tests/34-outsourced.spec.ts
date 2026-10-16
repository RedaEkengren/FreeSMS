import { test, expect } from '@playwright/test';
import { tech, desk, signIn, signOut, getApproved } from './helpers';

// Work sent to a supplier: asked for, sent, late, back, accepted by the shop
// and not before, with what it cost and their invoice handed on. The job is
// not ready while the work is out; the technician sees where it is and no money.

test('a turbocharger sent out, late, back, accepted, and the invoice handed on', async ({ page }) => {
  test.setTimeout(150_000);
  await signIn(page, desk);
  await page.goto('/jobs/new');
  await page.fill('input[name=registration]', 'TUR123');
  await page.fill('[name=complaint]', 'Tappar effekt');
  await page.getByRole('button', { name: 'Open the job' }).click();
  await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
  const job = page.url();
  await page.getByRole('link', { name: 'Say who is paying' }).click();
  await page.fill('form[action$="/customer"] input[name=name]', 'Turid Turbo');
  await page.getByRole('button', { name: 'Save and use' }).click();
  await expect(page).toHaveURL(job);
  await page.fill('input[form=add-line][name=description]', 'Renovera turbon');
  await page.fill('input[form=add-line][name=unit_price]', '6500');
  await page.locator('button[form=add-line]').click();
  await expect(page.locator('table.lines-table tbody tr', { hasText: 'Renovera turbon' })).toBeVisible();
  await getApproved(page);

  // Sent to the specialist, promised for yesterday.
  await page.goto(job);
  await page.getByRole('link', { name: 'Work at suppliers' }).click();
  const ask = page.locator('form:has(input[value=request])');
  await ask.locator('input[name=supplier]').fill('Turbo-Verkstan AB');
  await ask.locator('input[name=scope]').fill('Renovera och balansera');
  await ask.locator('input[name=item]').fill('Turboaggregatet');
  await ask.locator('input[name=estimate]').fill('4500');
  await ask.locator('input[name=expected_back]').fill('2020-03-01');
  await ask.getByRole('button', { name: 'Send it out' }).click();
  const record = () => page.locator('form:has(input[value=record])');
  await record().locator('select[name=kind]').selectOption({ label: 'sent' });
  await record().getByRole('button', { name: 'Write it down' }).click();
  await expect(page.getByText('at the supplier').first()).toBeVisible();
  await expect(page.getByText('overdue').first()).toBeVisible();
  await page.goto('/mine');
  await expect(page.getByText('Due back from a supplier and not here: Turbo-Verkstan AB: Turboaggregatet')).toBeVisible();
  await signOut(page);

  // The technician sees where it is, and no money; and cannot make the car ready.
  await signIn(page, tech);
  await page.goto(job);
  await page.getByRole('button', { name: 'Start work' }).click();
  await page.getByRole('link', { name: 'Work at suppliers' }).click();
  await expect(page.getByText('Turboaggregatet').first()).toBeVisible();
  await expect(page.getByText('at the supplier').first()).toBeVisible();
  await expect(page.getByText('4,500')).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Send it out' })).toHaveCount(0);
  await page.goto(job);
  await page.getByRole('button', { name: 'It is ready' }).click();
  await expect(page.getByText(/not back and checked yet/)).toBeVisible();
  await signOut(page);

  // It is late, then back, and back is not accepted.
  await signIn(page, desk);
  await page.goto(job + '/outsourced');
  await record().locator('select[name=kind]').selectOption({ label: 'waiting for them' });
  await record().locator('input[name=note]').fill('Balanseringsmaskinen står still');
  await record().locator('input[name=expected_back]').fill('2030-03-01');
  await record().getByRole('button', { name: 'Write it down' }).click();
  await expect(page.getByText('overdue')).toHaveCount(0);
  await record().locator('select[name=kind]').selectOption({ label: 'back from them' });
  await record().locator('select[name=outcome]').selectOption({ label: 'repaired' });
  await record().locator('input[name=note]').fill('Nytt kärnaggregat, balanserad enligt spec');
  await record().getByRole('button', { name: 'Write it down' }).click();
  await expect(page.getByText('back from them').first()).toBeVisible();
  await page.goto(job);
  await page.getByRole('button', { name: 'It is ready' }).click();
  await expect(page.getByText(/not back and checked yet/)).toBeVisible();

  // Only the shop's own word accepts it.
  await page.goto(job + '/outsourced');
  await record().locator('select[name=kind]').selectOption({ label: 'checked and accepted' });
  await record().locator('input[name=note]').fill('Snurrar fritt, inget glapp');
  await record().getByRole('button', { name: 'Write it down' }).click();
  await expect(page.getByText('checked and accepted').first()).toBeVisible();

  // What it cost, what they invoiced, and handing it on.
  await record().locator('select[name=kind]').selectOption({ label: 'cost recorded' });
  await record().locator('input[name=amount]').fill('4700');
  await record().getByRole('button', { name: 'Write it down' }).click();
  await expect(page.getByText('not the estimate')).toBeVisible();
  await record().locator('select[name=kind]').selectOption({ label: 'supplier invoice noted' });
  await record().locator('input[name=invoice_ref]').fill('F-8841');
  await record().getByRole('button', { name: 'Write it down' }).click();
  await expect(page.getByText('not handed on')).toBeVisible();
  await page.goto('/outsourced');
  await expect(page.getByText('invoice not handed on')).toBeVisible();
  await page.goto(job + '/outsourced');
  await record().locator('select[name=kind]').selectOption({ label: 'handed to the accounts' });
  await record().getByRole('button', { name: 'Write it down' }).click();
  await expect(page.getByText('handed to the accounts').first()).toBeVisible();
  await page.goto('/outsourced');
  await expect(page.getByText('Nothing is out with a supplier')).toBeVisible();
  await signOut(page);

  // Now the technician can finish.
  await signIn(page, tech);
  await page.goto(job);
  await page.getByRole('button', { name: 'It is ready' }).click();
  await expect(page.getByText('Ready').first()).toBeVisible();
  await signOut(page);
});
