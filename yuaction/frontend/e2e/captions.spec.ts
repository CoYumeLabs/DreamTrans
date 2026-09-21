import { expect, test } from "@playwright/test";
import { captionFeed } from "../src/captionFeed";

test("shared caption adapter preserves missing translations, CJK text and source boundaries", () => {
  const base = {
    source: "yufolo" as const,
    createdAt: "2026-09-21T00:00:00Z",
    speaker: "S1",
  };
  const fragments = [
    {
      ...base,
      id: "a",
      text: "你 好",
      startTime: 0,
      endTime: 0.1,
      translations: { en: "Hello" },
    },
    { ...base, id: "b", text: "世 界 。", startTime: 0.1, endTime: 0.2 },
  ];
  const pending = captionFeed(fragments, "en");
  expect(pending).toHaveLength(1);
  expect(pending[0].text).toBe("你好世界。");
  expect(pending[0].translations?.en).toBe("Hello");
  expect(pending[0].translationPending).toBe(true);
  const complete = captionFeed(
    [fragments[0], { ...fragments[1], translations: { en: "world." } }],
    "en",
  );
  expect(complete[0].translations?.en).toBe("Hello world.");
  expect(complete[0].translationPending).toBe(false);
  expect(
    captionFeed([
      ...fragments,
      {
        ...base,
        id: "demo",
        text: "Separate demo",
        source: "demo",
        startTime: 0.2,
        endTime: 0.3,
      },
    ]),
  ).toHaveLength(2);
});

test("fragmented history, live updates, reload and display use the same Yufolo cards", async ({
  page,
  browser,
}) => {
  const response = await page.request.post("/api/rooms", {
    data: { title: "Caption regression", kind: "classroom" },
  });
  expect(response.status()).toBe(201);
  const { room, hostKey } = await response.json();
  const base = `/api/rooms/${room.code}`;
  const parts = [
    "Hi.",
    "Hello.",
    "Could",
    "you",
    "hear",
    "me",
    "?",
    "Today",
    "we",
    "will",
    "discuss",
    "design.",
  ];
  await page.goto(`/rooms/${room.code}`);
  for (const [i, text] of parts.entries()) {
    const result = await page.request.post(`${base}/demo-segments`, {
      headers: { Authorization: `Bearer ${hostKey}` },
      data: { id: `fragment-${i}`, text, translation: `译${i}` },
    });
    expect(result.status()).toBe(200);
    await expect(page.locator(".caption")).toHaveCount(1);
  }
  const text = "Hi. Hello. Could you hear me? Today we will discuss design.";
  await expect(page.locator(".caption > p").first()).toHaveText(text);
  await expect(page.locator(".caption .translation")).toHaveText(
    parts.map((_, i) => `译${i}`).join(""),
  );
  await page.reload();
  await expect(page.locator(".caption")).toHaveCount(1);
  await expect(page.locator(".caption > p").first()).toHaveText(text);
  await page.getByRole("button", { name: `针对字幕提问：${text}` }).click();
  await expect(page.locator(".quote-preview")).toContainText(text);
  await page.getByLabel("你的问题").fill("Explain the whole sentence");
  await page.getByRole("button", { name: "发送问题" }).click();
  const snapshot = await (await page.request.get(base)).json();
  expect(snapshot.questions[0].quotedText).toBe(text);
  expect(snapshot.questions[0].segmentIds).toEqual(
    parts.map((_, i) => `fragment-${i}`),
  );
  expect(snapshot.segments).toHaveLength(parts.length);
  const screen = await browser.newPage();
  await screen.goto(`/rooms/${room.code}/display`);
  await expect(screen.locator(".display-caption > p").first()).toHaveText(text);
  await screen.close();
});

test("Yufolo sentence boundaries, speaker changes and delayed finals retain chronological cards", async ({
  page,
}) => {
  const text =
    "This completed explanation is long enough to form a readable paragraph, and the next sentence should start a fresh caption card.";
  const fragments = [
    { id: "one", text, startTime: 0, endTime: 4, speaker: "S1" },
    {
      id: "two",
      text: "Next sentence.",
      startTime: 4.1,
      endTime: 5,
      speaker: "S1",
    },
    {
      id: "three",
      text: "Another speaker.",
      startTime: 5.1,
      endTime: 6,
      speaker: "S2",
    },
    {
      id: "four",
      text: "A much later thought.",
      startTime: 30,
      endTime: 31,
      speaker: "S2",
    },
    {
      id: "late",
      text: "A delayed final.",
      startTime: 10,
      endTime: 11,
      speaker: "S2",
    },
  ];
  const room = {
    code: "ABCD1234",
    title: "Boundaries",
    kind: "classroom",
    status: "live",
    revision: 1,
    createdAt: new Date().toISOString(),
    questions: [],
    segments: fragments.map((s) => ({
      ...s,
      source: "yufolo",
      createdAt: new Date().toISOString(),
    })),
  };
  await page.route("**/api/rooms/ABCD1234", (route) =>
    route.fulfill({ json: room }),
  );
  await page.route("**/api/rooms/ABCD1234/events", (route) =>
    route.fulfill({
      contentType: "text/event-stream",
      body: `event: room\ndata: ${JSON.stringify(room)}\n\n`,
    }),
  );
  await page.goto("/rooms/ABCD1234");
  await expect(page.locator(".caption > p:first-of-type")).toHaveText([
    text,
    "Next sentence.",
    "Another speaker.",
    "A delayed final.",
    "A much later thought.",
  ]);
});
