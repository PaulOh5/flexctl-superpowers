import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: './e2e',
  timeout: 60_000,
  retries: 0,
  use: {
    baseURL: 'http://localhost:8080',
    trace: 'on-first-retry',
  },
  // control-plane은 외부에서 띄움 (Makefile e2e 타깃에서 처리).
})
