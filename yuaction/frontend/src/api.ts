export type Question = {
  id: string;
  content: string;
  status: "pending" | "showing" | "answered";
  segmentId?: string;
  quotedText?: string;
  createdAt: string;
};
export type Segment = {
  id: string;
  text: string;
  translation?: string;
  source: "demo" | "yufolo";
  createdAt: string;
  updatedAt?: string;
  speaker?: string;
  segmentIds?: readonly string[];
  startTime?: number;
  endTime?: number;
  translations?: Record<string, string>;
  translationErrors?: Record<string, string>;
};
export type Room = {
  code: string;
  title: string;
  kind: "classroom" | "talk";
  status: "live" | "ended";
  revision: number;
  createdAt: string;
  questions: Question[];
  segments: Segment[];
  transcription?: string;
};
export type Config = {
  demo: boolean;
  creatorKeyRequired: boolean;
  yufoloConnected: boolean;
};

export async function api<T>(
  path: string,
  options: {
    method?: string;
    body?: unknown;
    key?: string;
    signal?: AbortSignal;
  } = {},
): Promise<T> {
  const response = await fetch(`/api${path}`, {
    method: options.method || "GET",
    headers: {
      ...(options.body !== undefined
        ? { "Content-Type": "application/json" }
        : {}),
      ...(options.key ? { Authorization: `Bearer ${options.key}` } : {}),
    },
    body: options.body !== undefined ? JSON.stringify(options.body) : undefined,
    signal: options.signal ?? AbortSignal.timeout(15000),
  });
  const result = await response.json();
  if (!response.ok) throw new Error(result.error || "请求失败，请稍后重试");
  return result as T;
}

export function readHostKey(code: string) {
  return sessionStorage.getItem(`yuaction.host.${code}`) || "";
}
export function saveHostKey(code: string, key: string) {
  sessionStorage.setItem(`yuaction.host.${code}`, key);
}
export type RecentRoom = Pick<Room, "code" | "title" | "kind" | "createdAt">;
export function recentRooms(): RecentRoom[] {
  try {
    const value: unknown = JSON.parse(
      localStorage.getItem("yuaction.recent") || "[]",
    );
    if (!Array.isArray(value)) return [];
    return value
      .filter(
        (r): r is RecentRoom =>
          !!r &&
          typeof r.code === "string" &&
          /^[A-F0-9]{8}$/.test(r.code) &&
          typeof r.title === "string" &&
          (r.kind === "classroom" || r.kind === "talk") &&
          typeof r.createdAt === "string",
      )
      .slice(0, 12);
  } catch {
    return [];
  }
}
export function rememberRoom(room: Room) {
  // Only room metadata is persisted; host credentials stay in this tab.
  localStorage.setItem(
    "yuaction.recent",
    JSON.stringify(
      [
        {
          code: room.code,
          title: room.title,
          kind: room.kind,
          createdAt: room.createdAt,
        },
        ...recentRooms().filter((r) => r.code !== room.code),
      ].slice(0, 12),
    ),
  );
}
