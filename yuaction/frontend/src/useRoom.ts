import { useEffect, useState } from "react";
import { api, type Room } from "./api";

export function useRoom(code: string) {
  const [room, setRoom] = useState<Room | null>(null);
  const [connection, setConnection] = useState<
    "connecting" | "live" | "reconnecting"
  >("connecting");
  const [error, setError] = useState("");
  useEffect(() => {
    let active = true;
    const controller = new AbortController();
    const accept = (next: Room) => {
      if (active)
        setRoom((old) => (!old || next.revision >= old.revision ? next : old));
    };
    api<Room>(`/rooms/${code}`, { signal: controller.signal })
      .then(accept)
      .catch((e) => {
        if (active) setError(e.message);
      });
    let events: EventSource | undefined;
    const connect = () => {
      if (!active) return;
      const previous = events;
      const next = new EventSource(`/api/rooms/${code}/events`);
      events = next;
      previous?.close();
      next.addEventListener("room", (event) => {
        if (events !== next || !active) return;
        try {
          accept(JSON.parse((event as MessageEvent).data));
          setError("");
          setConnection("live");
        } catch {
          setError("无法读取实时数据，请刷新页面");
        }
      });
      next.addEventListener("handoff", (event) => {
        if (events === next && active && (event as MessageEvent).data === "1") {
          // The route has already switched. Reconnect immediately while keeping
          // the last snapshot and live indicator; genuine errors still surface.
          connect();
        }
      });
      next.onerror = () => {
        if (active && events === next) setConnection("reconnecting");
      };
      next.onopen = () => {
        if (active && events === next) setConnection("live");
      };
    };
    connect();
    return () => {
      active = false;
      controller.abort();
      events?.close();
    };
  }, [code]);
  function accept(next: Room) {
    setRoom((old) => (!old || next.revision >= old.revision ? next : old));
  }
  return { room, connection, error, accept };
}
