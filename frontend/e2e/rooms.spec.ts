import { expect, test } from "@playwright/test";

test("host, two participants and display share questions and captions", async ({
  browser,
  page,
}) => {
  const consoleErrors: string[] = [];
  page.on("pageerror", (e) => consoleErrors.push(e.message));
  await page.goto("/");
  await page.getByRole("button", { name: "创建活动", exact: true }).click();
  await page.getByLabel("活动名称").fill("设计思维 · 第一堂课");
  await page.getByRole("button", { name: "创建并进入工作台" }).click();
  await expect(
    page.getByRole("heading", { name: "设计思维 · 第一堂课", exact: true }),
  ).toBeVisible();
  const code = page.url().match(/rooms\/([A-F0-9]+)\/host/)![1];
  const one = await browser.newContext({
    viewport: { width: 390, height: 844 },
  });
  const two = await browser.newContext();
  const displayContext = await browser.newContext();
  const p1 = await one.newPage(),
    p2 = await two.newPage(),
    screen = await displayContext.newPage();
  await Promise.all([
    p1.goto(`/rooms/${code}`),
    p2.goto(`/rooms/${code}`),
    screen.goto(`/rooms/${code}/display`),
  ]);
  await p1.getByLabel("你的问题").fill("可以用一个真实的案例解释吗？");
  await p1.getByRole("button", { name: "发送问题" }).click();
  await expect(page.locator(".question-card")).toContainText(
    "可以用一个真实的案例解释吗？",
  );
  await expect(p2.locator(".question-card")).toContainText(
    "可以用一个真实的案例解释吗？",
  );
  await expect(p2.getByRole("button", { name: "展示问题" })).toHaveCount(0);
  await page.getByRole("button", { name: "展示问题" }).click();
  await expect(
    screen.getByRole("heading", { name: "可以用一个真实的案例解释吗？" }),
  ).toBeVisible();
  await page.getByLabel("演示字幕原文").fill("我们从观察用户的真实需求开始。");
  await page
    .getByLabel("演示字幕译文")
    .fill("We start by observing real user needs.");
  await page.getByRole("button", { name: "发送演示字幕" }).click();
  await expect(p1.locator(".caption-list")).toContainText(
    "我们从观察用户的真实需求开始。",
  );
  await expect(p2.locator(".caption-list")).toContainText(
    "We start by observing real user needs.",
  );
  await expect(screen.locator(".display-caption")).toContainText(
    "我们从观察用户的真实需求开始。",
  );
  await p1.getByRole("button", { name: /针对字幕提问/ }).click();
  await expect(p1.locator(".quote-preview")).toContainText("观察用户");
  await p1.getByLabel("你的问题").fill("如何区分真实需求和表面需求？");
  await p1.getByRole("button", { name: "发送问题" }).click();
  await expect(page.locator(".question-card").first()).toContainText(
    "来自一段共享字幕",
  );
  await one.setOffline(true);
  await p2.getByLabel("你的问题").fill("离线期间提出的新问题");
  await p2.getByRole("button", { name: "发送问题" }).click();
  await one.setOffline(false);
  await p1.reload();
  await expect(p1.locator(".question-list")).toContainText(
    "离线期间提出的新问题",
  );
  expect(
    await p1.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBeTruthy();
  await page.screenshot({ path: "test-results/host.png", fullPage: true });
  await p1.screenshot({
    path: "test-results/participant-mobile.png",
    fullPage: true,
  });
  await screen.screenshot({ path: "test-results/display.png", fullPage: true });
  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "结束活动" }).click();
  await expect(p1.getByRole("button", { name: "活动已结束" })).toBeDisabled();
  await expect(screen.getByText("活动已结束", { exact: true })).toBeVisible();
  expect(consoleErrors).toEqual([]);
  await Promise.all([one.close(), two.close(), displayContext.close()]);
});

test("home layout and host access gate work on mobile", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/");
  await expect(
    page.getByRole("heading", { name: "让每一次表达，都有回应。" }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBeTruthy();
  await page.screenshot({
    path: "test-results/home-mobile.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.screenshot({
    path: "test-results/home-desktop.png",
    fullPage: true,
  });
  const result = await page.request.post("/api/rooms", {
    data: { title: "访问验证", kind: "talk" },
  });
  const { room, hostKey } = await result.json();
  await page.goto(`/rooms/${room.code}/host`);
  await expect(
    page.getByRole("heading", { name: "进入主持人工作台" }),
  ).toBeVisible();
  await page.getByLabel("主持人密钥", { exact: true }).fill("wrong-key");
  await page.getByRole("button", { name: "进入工作台" }).click();
  await expect(page.getByRole("alert")).toContainText("需要此房间的主持人密钥");
  await page.getByLabel("主持人密钥", { exact: true }).fill(hostKey);
  await page.getByRole("button", { name: "进入工作台" }).click();
  await expect(
    page.getByRole("heading", { name: "访问验证", exact: true }),
  ).toBeVisible();
});
