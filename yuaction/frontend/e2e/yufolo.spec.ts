import { test, expect } from "@playwright/test";

test("Yufolo login, one microphone, shared captions, pause and account recovery", async ({
  page,
  browser,
}) => {
  await page.goto("/");
  await page.screenshot({
    path: "test-results/yufolo-home.png",
    fullPage: true,
  });
  await page.getByRole("button", { name: "创建活动", exact: true }).click();
  await expect(
    page.getByText("先登录，随后继续创建你的活动。", { exact: true }),
  ).toBeVisible();
  await expect(page.getByLabel("邮箱", { exact: true })).toBeFocused();
  await page.getByLabel("邮箱", { exact: true }).fill("teacher@example.com");
  await page.getByLabel("密码", { exact: true }).fill("test-password");
  await page.getByRole("button", { name: "登录 Yufolo", exact: true }).click();
  await expect(
    page.getByText("已连接 Yufolo · 活动归属于此账号"),
  ).toBeVisible();
  await expect(page.getByRole("dialog")).toBeVisible();
  const title = "设计思维 · 让问题成为起点";
  await page.getByLabel("活动名称", { exact: true }).fill(title);
  await page.getByRole("button", { name: "创建并进入工作台" }).click();
  await expect(page).toHaveURL(/\/rooms\/[A-F0-9]{8}\/host/);
  const hostURL = page.url();
  await expect(
    page.getByRole("button", { name: "开始转录", exact: true }),
  ).toBeVisible();
  const guestURL = hostURL.replace(/\/host$/, "");
  const guestContext = await browser.newContext({ locale: "zh-CN" });
  const guest = await guestContext.newPage();
  await guest.goto(guestURL);
  await expect(guest.getByRole("heading", { name: title })).toBeVisible();
  await expect(guest.getByRole("button", { name: "开始转录" })).toHaveCount(0);
  await expect(page.getByLabel("原文语言", { exact: true })).toHaveValue("cmn");
  await expect(page.getByLabel("共享译文", { exact: true })).toHaveCount(0);
  await guest.getByLabel("我的译文语言").selectOption("en");
  await page.getByRole("button", { name: "开始转录", exact: true }).click();
  await expect(page.getByText("正在采集麦克风")).toBeVisible();
  await expect(guest.getByText("欢迎来到课堂", { exact: true })).toBeVisible();
  await expect(guest.getByText("Hello everyone", { exact: true })).toBeVisible({
    timeout: 15000,
  });
  await page.screenshot({
    path: "test-results/yufolo-host.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: "test-results/yufolo-host-mobile.png",
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBeTruthy();
  await page.setViewportSize({ width: 1280, height: 720 });
  const recoveredContext = await browser.newContext({ locale: "zh-CN" });
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
  await expect(
    recovered.getByText("其他主持端正在转录", { exact: true }),
  ).toBeVisible();
  await expect(
    recovered.getByRole("button", { name: "正在接收共享字幕", exact: true }),
  ).toBeDisabled();
  await page.getByRole("button", { name: "暂停转录", exact: true }).click();
  await expect(page.getByText("麦克风未开启")).toBeVisible();
  await expect(
    recovered.getByRole("button", { name: "开始转录", exact: true }),
  ).toBeEnabled();
  recovered.on("dialog", (dialog) => dialog.accept());
  await recovered
    .getByRole("button", { name: "结束活动", exact: true })
    .click();
  await expect(
    guest.getByText("活动已结束", { exact: true }).first(),
  ).toBeVisible();
  await recovered.goto("/");
  await expect(recovered.getByRole("heading", { name: title })).toBeVisible();
  await expect(recovered.locator(".hero-card")).not.toBeVisible();
  await recovered.screenshot({
    path: "test-results/yufolo-workspace.png",
    fullPage: true,
  });
  await recovered.setViewportSize({ width: 390, height: 844 });
  await recovered.screenshot({
    path: "test-results/yufolo-workspace-mobile.png",
    fullPage: true,
  });
  expect(
    await recovered.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBeTruthy();
  await guestContext.close();
  await recoveredContext.close();
});

test("Host source language persists when pausing and resuming the same room", async ({
  page,
}) => {
  await page.goto("/");
  await page.getByLabel("邮箱", { exact: true }).fill("teacher@example.com");
  await page.getByLabel("密码", { exact: true }).fill("test-password");
  await page.getByRole("button", { name: "登录 Yufolo", exact: true }).click();
  await expect(
    page.getByText("已连接 Yufolo · 活动归属于此账号"),
  ).toBeVisible();
  await page.getByRole("button", { name: "创建活动", exact: true }).click();
  await page
    .getByLabel("活动名称", { exact: true })
    .fill("English to Chinese regression");
  await page.getByRole("button", { name: "创建并进入工作台" }).click();
  await page.getByLabel("原文语言", { exact: true }).selectOption("en");
  await expect(page.getByLabel("共享译文", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "开始转录", exact: true }).click();
  await expect(page.getByText("正在采集麦克风")).toBeVisible();
  await page.getByRole("button", { name: "暂停转录", exact: true }).click();
  await expect(page.getByText("麦克风未开启")).toBeVisible();
  await page.reload();
  await expect(page.getByLabel("原文语言", { exact: true })).toHaveValue("en");
  await expect(page.getByLabel("原文语言", { exact: true })).toBeDisabled();
  await page.getByRole("button", { name: "开始转录", exact: true }).click();
  await expect(page.getByText("正在采集麦克风")).toBeVisible();
  await page.getByRole("button", { name: "暂停转录", exact: true }).click();
  await expect(page.getByText("麦克风未开启")).toBeVisible();
});

test("Yufolo knowledge upload, private automatic AI drafts, indexing and deletion", async ({
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
  await page.getByLabel("活动名称", { exact: true }).fill("AI classroom");
  await page.getByRole("button", { name: "创建并进入工作台" }).click();
  await expect(page.getByRole("heading", { name: "资料与 AI" })).toBeVisible();
  await page.getByLabel("上传知识库资料").setInputFiles({
    name: "lecture.txt",
    mimeType: "text/plain",
    buffer: Buffer.from("设计思维从理解用户开始。"),
  });
  await expect(page.getByText("lecture.txt", { exact: true })).toBeVisible();
  await page.getByText("知识库与回答设置", { exact: true }).click();
  await page.getByLabel("收到新问题时自动生成 AI 建议").check();
  await page.getByLabel("通用回答提示词").fill("请给出适合教学的例子。");
  await page.getByRole("button", { name: "保存回答设置" }).click();
  await expect(page.getByText("设置已保存。")).toBeVisible();
  await page.getByRole("button", { name: "建立语义索引", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("0.02 DP");
  await page.getByRole("button", { name: "确认并建立索引" }).click();
  await expect(page.getByRole("dialog")).not.toBeVisible();
  const context = await browser.newContext({ locale: "zh-CN" });
  const guest = await context.newPage();
  await guest.goto(page.url().replace(/\/host$/, ""));
  await expect(guest.getByRole("heading", { name: "资料与 AI" })).toHaveCount(
    0,
  );
  await guest
    .getByPlaceholder("有什么疑问，或者想进一步了解的地方？")
    .fill("设计思维应该从哪里开始？");
  await guest.getByRole("button", { name: "发送问题" }).click();
  await expect(
    page.getByText("通用建议：先明确概念，再举一个例子。"),
  ).toBeVisible({ timeout: 10000 });
  await expect(
    page.getByText("根据讲义，设计思维从理解用户开始。"),
  ).toBeVisible();
  await expect(
    guest.getByText("根据讲义，设计思维从理解用户开始。"),
  ).toHaveCount(0);
  await page.reload();
  await page.getByText("知识库与回答设置", { exact: true }).click();
  await expect(page.getByLabel("通用回答提示词")).toHaveValue(
    "请给出适合教学的例子。",
  );
  page.on("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "删除资料 lecture.txt" }).click();
  await expect(page.locator(".document-list")).toHaveCount(0);
  await expect(page.getByText("资料已变更，请重新生成")).toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBeTruthy();
  await page.screenshot({
    path: "test-results/yufolo-ai-mobile.png",
    fullPage: true,
  });
  await context.close();
});
