import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  workers: 1,
  timeout: 45000,
  use: {
    baseURL: "http://127.0.0.1:5175",
    trace: "retain-on-failure",
    ...devices["Desktop Chrome"],
  },
  webServer: [
    {
      command:
        "cd ../backend && YUACTION_DEMO=true LISTEN_ADDR=127.0.0.1:18084 go run ./cmd/server",
      url: "http://127.0.0.1:18084/api/health",
      reuseExistingServer: false,
      timeout: 120000,
    },
    {
      command: "API_TARGET=http://127.0.0.1:18084 npm run dev -- --port 5175",
      url: "http://127.0.0.1:5175",
      reuseExistingServer: false,
    },
  ],
});
