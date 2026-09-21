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
  projects: [
    { name: "demo", testIgnore: /yufolo\.spec\.ts/ },
    {
      name: "yufolo",
      testMatch: /yufolo\.spec\.ts/,
      use: {
        baseURL: "http://127.0.0.1:5176",
        permissions: ["microphone"],
        launchOptions: {
          args: [
            "--use-fake-device-for-media-stream",
            "--use-fake-ui-for-media-stream",
          ],
        },
      },
    },
  ],
  webServer: [
    {
      command:
        "cd ../backend && go test -tags=e2e ./internal/app -run '^TestBrowserFixture$' -v -timeout 0",
      url: "http://127.0.0.1:18086/api/health",
      reuseExistingServer: false,
      timeout: 120000,
    },
    {
      command: "API_TARGET=http://127.0.0.1:18086 npm run dev -- --port 5176",
      url: "http://127.0.0.1:5176",
      reuseExistingServer: false,
    },
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
