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
    const events = new EventSource(`/api/rooms/${code}/events`);
    events.addEventListener("room", (event) => {
      try {
        accept(JSON.parse((event as MessageEvent).data));
        setError("");
        setConnection("live");
      } catch {
        setError("无法读取实时数据，请刷新页面");
      }
    });
    events.onerror = () => {
      if (active) setConnection("reconnecting");
    };
    events.onopen = () => {
      if (active) setConnection("live");
    };
    return () => {
      active = false;
      controller.abort();
      events.close();
    };
  }, [code]);
  function accept(next: Room) {
    setRoom((old) => (!old || next.revision >= old.revision ? next : old));
  }
  return { room, connection, error, accept };
}
