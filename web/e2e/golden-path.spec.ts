import { test, expect } from '@playwright/test'
import { execSync } from 'node:child_process'

test('signup → create env → SSH command visible', async ({ page }) => {
  const stamp = Date.now()
  const slug = `tester${stamp}`

  // 1. signup
  await page.goto('/signup')
  await page.fill('input[name="email"]', `test-${stamp}@x.com`)
  await page.fill('input[name="slug"]', slug)
  await page.fill('input[name="password"]', 'supersecret123')
  await page.click('button[type="submit"]')
  await page.waitForURL('http://localhost:8080/')

  // Seed an e2e-node for this fresh user (so node dropdown is non-empty).
  execSync(`./scripts/e2e-seed.sh ${slug}`, { cwd: '..', stdio: 'inherit' })

  // 2. empty dashboard
  await expect(page.locator('text=No environments yet')).toBeVisible()

  // 3. create env
  await page.click('text=New env')
  await page.waitForURL(/\/envs\/new$/)
  await page.fill('input[name="name"]', 'cuda')
  await page.selectOption('select[name="template_id"]', 'cuda-base')
  await page.selectOption('select[name="node_id"]', { index: 1 }) // 0 is "— select —"
  await page.fill('input[name="gpu_request"]', '1')
  await page.click('button[type="submit"]')

  // 4. env detail, status creating → running (autoack ~500ms)
  await page.waitForURL(/\/envs\/[\w-]+$/)
  await expect(page.locator('text=running').first()).toBeVisible({ timeout: 10_000 })

  // 5. both SSH commands rendered
  await expect(page.locator('text=flexctl ssh cuda')).toBeVisible()
  await expect(page.locator('text=/ssh dev@.+\\.flex/')).toBeVisible()
})
