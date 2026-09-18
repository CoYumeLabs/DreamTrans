import { test, expect } from "@playwright/test";

test("Yufolo login, one microphone, shared captions, pause and account recovery", async ({
  page,
  browser,
}) => {
  await page.goto("/");
  await page.getByLabel("邮箱", { exact: true }).fill("teacher@example.com");
  await page.getByLabel("密码", { exact: true }).fill("test-password");
  await page.getByRole("button", { name: "登录 Yufolo", exact: true }).click();
  await expect(
    page.getByText("已连接 Yufolo · 活动归属于此账号"),
  ).toBeVisible();
  await page.getByRole("button", { name: "创建活动", exact: true }).click();
  const title = `共享转录-${Date.now()}`;
  await page.getByLabel("活动名称", { exact: true }).fill(title);
  await page.getByRole("button", { name: "创建并进入工作台" }).click();
  await expect(page).toHaveURL(/\/rooms\/[A-F0-9]{8}\/host/);
  const hostURL = page.url();
  const guestURL = hostURL.replace(/\/host$/, "");
  const guestContext = await browser.newContext();
  const guest = await guestContext.newPage();
  await guest.goto(guestURL);
  await expect(guest.getByRole("heading", { name: title })).toBeVisible();
  await expect(guest.getByRole("button", { name: "开始转录" })).toHaveCount(0);
  await page.getByLabel("共享译文", { exact: true }).selectOption("en");
  await page.getByRole("button", { name: "开始转录", exact: true }).click();
  await expect(page.getByText("正在采集麦克风")).toBeVisible();
  await expect(guest.getByText("欢迎来到课堂", { exact: true })).toBeVisible();
  await expect(
    guest.getByText("Hello everyone", { exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "暂停转录", exact: true }).click();
  await expect(page.getByText("麦克风未开启")).toBeVisible();
  const recoveredContext = await browser.newContext();
  const recovered = await recoveredContext.newPage();
  await recovered.goto(hostURL);
  await recovered
    .getByLabel("邮箱", { exact: true })
    .fill("teacher@example.com");
  await recovered.getByLabel("密码", { exact: true }).fill("test-password");
  await recovered
    .getByRole("button", { name: "登录 Yufolo", exact: true })
    .click();
  await expect(recovered.getByRole("heading", { name: title })).toBeVisible();
  await expect(
    recovered.getByText("欢迎来到课堂", { exact: true }),
  ).toBeVisible();
  recovered.on("dialog", (dialog) => dialog.accept());
  await recovered
    .getByRole("button", { name: "结束活动", exact: true })
    .click();
  await expect(
    guest.getByText("活动已结束", { exact: true }).first(),
  ).toBeVisible();
  await guestContext.close();
  await recoveredContext.close();
});
