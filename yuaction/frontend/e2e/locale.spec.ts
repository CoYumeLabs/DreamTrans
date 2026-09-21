import { expect, test } from "@playwright/test";

test("a Chinese browser can switch to English and keep the choice", async ({
  page,
}) => {
  await page.goto("/");
  await expect(page.locator("html")).toHaveAttribute("lang", "zh-CN");
  await expect(
    page.getByRole("heading", { name: "让每一次表达，都有回应。" }),
  ).toBeVisible();

  await page.getByRole("radio", { name: "English" }).click();
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await expect(
    page.getByRole("heading", { name: "Every expression deserves a response." }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Create activity", exact: true })).toBeVisible();

  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await expect(
    page.getByRole("heading", { name: "Every expression deserves a response." }),
  ).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem("ya_locale"))).toBe("en");
});

test.describe("english browser", () => {
  test.use({ locale: "en-US" });

  test("opens in English until Chinese is chosen", async ({ page }) => {
    await page.goto("/");
    await expect(page.locator("html")).toHaveAttribute("lang", "en");
    await expect(page.getByRole("button", { name: "Join", exact: true })).toBeVisible();
    await page.getByRole("radio", { name: "中文" }).click();
    await expect(page.getByRole("button", { name: "加入", exact: true })).toBeVisible();
  });
});
