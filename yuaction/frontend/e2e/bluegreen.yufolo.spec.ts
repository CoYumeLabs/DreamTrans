import { test, expect } from "@playwright/test";

test("recording survives upgrade, rollback and stop during handoff without lost or repeated PCM", async ({
  page,
}) => {
  await page.request.post("/api/__fixture/reset");
  await page.addInitScript(() => {
    const trace = {
      microphones: 0,
      tracks: [] as MediaStreamTrack[],
      frames: [] as Uint8Array[],
      worklets: 0,
    };
    Object.assign(window, { recordingTrace: trace });
    const original = navigator.mediaDevices.getUserMedia.bind(
      navigator.mediaDevices,
    );
    navigator.mediaDevices.getUserMedia = async (constraints) => {
      trace.microphones++;
      const stream = await original(constraints);
      trace.tracks.push(...stream.getTracks());
      return stream;
    };
    const Worklet = window.AudioWorkletNode;
    window.AudioWorkletNode = class extends Worklet {
      constructor(
        context: BaseAudioContext,
        name: string,
        options?: AudioWorkletNodeOptions,
      ) {
        super(context, name, options);
        trace.worklets++;
        this.port.addEventListener("message", (event) => {
          if (event.data instanceof ArrayBuffer)
            trace.frames.push(new Uint8Array(event.data).slice());
        });
      }
    };
  });
  await page.goto("/");
  await page.getByLabel("邮箱", { exact: true }).fill("teacher@example.com");
  await page.getByLabel("密码", { exact: true }).fill("test-password");
  await page.getByRole("button", { name: "登录 Yufolo", exact: true }).click();
  await page.getByRole("button", { name: "创建活动", exact: true }).click();
  await page
    .getByLabel("活动名称", { exact: true })
    .fill("Continuous recording deployment");
  await page.getByRole("button", { name: "创建并进入工作台" }).click();
  await page.getByRole("button", { name: "开始转录", exact: true }).click();
  await expect(page.getByText("正在采集麦克风", { exact: true })).toBeVisible();
  const stats = async () =>
    (await page.request.get("/api/__fixture/stats")).json();
  await expect.poll(async () => (await stats()).bytes).toBeGreaterThan(10000);
  // A candidate that cannot serve authenticated requests must leave the old
  // recording untouched, including its MediaStream and worklet.
  await page.request.post("/api/__fixture/fail-candidate");
  expect((await stats()).starts).toBe(1);
  await expect(page.locator(".connection")).not.toContainText("连接中断");
  await page.evaluate(() => {
    const interruptions: string[] = [];
    Object.assign(window, { deploymentInterruptions: interruptions });
    new MutationObserver(() => {
      const status = document.querySelector(".connection")?.textContent || "";
      if (status.includes("连接中断")) interruptions.push(status);
    }).observe(document.body, {
      subtree: true,
      childList: true,
      characterData: true,
    });
  });
  for (const expectedStarts of [2, 3]) {
    await page.request.post("/api/__fixture/deploy");
    await expect.poll(async () => (await stats()).starts).toBe(expectedStarts);
    await expect(
      page.getByText("正在采集麦克风", { exact: true }),
    ).toBeVisible();
    const continuity = await page.evaluate(() => {
      const trace = (
        window as unknown as {
          recordingTrace: {
            microphones: number;
            worklets: number;
            tracks: MediaStreamTrack[];
          };
        }
      ).recordingTrace;
      return {
        microphones: trace.microphones,
        worklets: trace.worklets,
        live: trace.tracks.every((track) => track.readyState === "live"),
      };
    });
    expect(continuity).toEqual({ microphones: 1, worklets: 1, live: true });
    expect((await page.request.get("/api/auth/me")).status()).toBe(200);
  }
  await page.request.post("/api/__fixture/deploy");
  await page.getByRole("button", { name: "暂停转录", exact: true }).click();
  await expect(page.getByText("麦克风未开启", { exact: true })).toBeVisible();
  await expect.poll(async () => (await stats()).connections).toBe(0);
  const captured = await page.evaluate(() => {
    const trace = (
      window as unknown as { recordingTrace: { frames: Uint8Array[] } }
    ).recordingTrace;
    let bytes = 0;
    let hash = 14695981039346656037n;
    for (const frame of trace.frames) {
      bytes += frame.byteLength;
      for (const value of frame)
        hash = BigInt.asUintN(64, (hash ^ BigInt(value)) * 1099511628211n);
    }
    return { bytes, hash: hash.toString(16).padStart(16, "0") };
  });
  const received = await stats();
  expect(received.bytes).toBe(captured.bytes);
  expect(received.hash).toBe(captured.hash);
  expect(received.maxConnections).toBe(1);
  expect(
    await page.evaluate(
      () =>
        (window as unknown as { deploymentInterruptions: string[] })
          .deploymentInterruptions,
    ),
  ).toEqual([]);
  expect(captured.bytes).toBeGreaterThan(10000);
});
