import { test, expect } from '@playwright/test';
import { owner, tech, desk, signIn, signOut } from './helpers';

// From an empty database to an invoice, through the forms and nothing else:
// no seed data, no SQL, every relationship made the way a workshop makes it.
// The Go suite proves the parts; this proves they join up for a person
// holding a browser.
test.describe.serial('a morning, from an empty database', () => {
  test('setting up the workshop signs the owner in', async ({ page }) => {
    await page.goto('/');
    await expect(page).toHaveURL(/\/setup/);
    await page.fill('input[name=shop_name]', 'Testverkstaden');
    await page.fill('input[name=owner_name]', owner.name);
    await page.fill('input[name=email]', owner.email);
    await page.fill('input[name=password]', owner.password);
    await page.getByRole('button', { name: 'Create the workshop' }).click();
    await expect(page).not.toHaveURL(/\/setup|\/login/);
    await expect(page.getByRole('link', { name: 'Staff' })).toBeVisible();
  });

  test('the owner adds a technician and a front desk', async ({ page }) => {
    await signIn(page, owner);
    for (const [who, role] of [[tech, 'technician'], [desk, 'service_advisor']] as const) {
      await page.goto('/staff');
      const add = page.locator('form[action="/staff"]');
      await add.locator('input[name=name]').fill(who.name);
      await add.locator('input[name=email]').fill(who.email);
      await add.locator('select[name=role]').selectOption(role);
      await add.locator('input[name=password]').fill(who.password);
      await page.getByRole('button', { name: 'Add them' }).click();
      await expect(page.getByText(who.name)).toBeVisible();
    }
  });

  test('the front desk takes a car in, names the customer and prices the job', async ({ page }) => {
    await signIn(page, desk);
    await page.goto('/jobs/new');
    await page.fill('input[name=registration]', 'ABC123');
    await page.fill('input[name=odometer_km]', '18 500');
    await page.fill('[name=complaint]', 'Skramlar fram vid 80');
    await page.getByRole('button', { name: 'Open the job' }).click();
    await expect(page).toHaveURL(/\/jobs\/[0-9a-f-]{36}$/);
    const job = page.url();

    await page.getByRole('link', { name: 'Say who is paying' }).click();
    await page.fill('form[action$="/customer"] input[name=name]', 'Karin Kund');
    await page.fill('form[action$="/customer"] input[name=phone]', '070-123 45 67');
    await page.check('form[action$="/customer"] input[name=also_owner]');
    await page.getByRole('button', { name: 'Save and use' }).click();
    await expect(page).toHaveURL(job);
    await expect(page.getByText('Karin Kund')).toBeVisible();

    // The job line: its inputs sit in the table, joined to the form by the
    // form attribute.
    await page.fill('input[form=add-line][name=description]', 'Byte av bromsskivor fram');
    await page.fill('input[form=add-line][name=quantity]', '1,5');
    await page.fill('input[form=add-line][name=unit_price]', '895');
    await page.locator('button[form=add-line]').click();
    await expect(page.getByText('Byte av bromsskivor fram')).toBeVisible();

    for (const action of ['Price it', 'Ask the customer', 'Mark it approved']) {
      await page.getByRole('button', { name: action }).click();
    }
    await expect(page.getByText('Approved, not started')).toBeVisible();
    await signOut(page);
  });

  test('the technician sees the job without the customer, works it and hands it back', async ({ page }) => {
    await signIn(page, tech);
    await page.getByRole('link', { name: /ABC\s?123/ }).first().click();
    await expect(page.getByText('Skramlar fram vid 80')).toBeVisible();
    // The vehicle and the job, not the customer.
    await expect(page.getByText('Karin Kund')).toHaveCount(0);
    await expect(page.getByText('070-123 45 67')).toHaveCount(0);

    await page.getByRole('button', { name: 'Start work' }).click();
    await page.getByRole('button', { name: 'Start clock' }).click();
    await expect(page.getByRole('button', { name: 'Stop clock' })).toBeVisible();
    await page.getByRole('button', { name: 'Stop clock' }).click();
    await page.getByRole('button', { name: 'It is ready' }).click();
    await expect(page.getByText('Ready for collection')).toBeVisible();
    await signOut(page);
  });

  test('the front desk invoices it', async ({ page }) => {
    await signIn(page, desk);
    await page.getByRole('link', { name: /ABC\s?123/ }).first().click();
    await page.getByRole('button', { name: 'Invoice it' }).click();
    // 1,5 h at 895,00 is 1 342,50; 25 per cent on top is 1 678,13.
    await page.getByRole('link', { name: /A-1/ }).first().click();
    await expect(page.getByText('Karin Kund')).toBeVisible();
    await expect(page.getByText('Byte av bromsskivor fram')).toBeVisible();
    await expect(page.getByText(/1[\s\u00a0,]?678[.,]13/)).toBeVisible();
  });
});
