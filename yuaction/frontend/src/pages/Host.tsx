import { useEffect, useState, type FormEvent } from "react";
import { ArrowRight, Settings2 } from "lucide-react";
import { api, readHostKey, saveHostKey } from "../api";
import { Brand, Button, ErrorNote } from "../components/ui";
import RoomPage from "./RoomPage";
import { useConfig } from "../hooks/useConfig";
import AccountPanel from "../components/AccountPanel";

function HostGate({
  code,
  onReady,
}: {
  code: string;
  onReady: (key: string) => void;
}) {
  const [key, setKey] = useState(readHostKey(code));
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  async function verify(e?: FormEvent) {
    e?.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api(`/rooms/${code}/host`, { key });
      saveHostKey(code, key);
      onReady(key);
    } catch (e) {
      setError((e as Error).message);
      setBusy(false);
    }
  }
  useEffect(() => {
    if (key) void verify();
  }, []);
  return (
    <div className="center-page">
      <Brand />
      <form className="panel gate-form" onSubmit={verify}>
        <span className="empty-icon">
          <Settings2 size={26} />
        </span>
        <h1>进入主持人工作台</h1>
        <p>房间 {code} · 输入创建活动时保存的主持人密钥。</p>
        <label>
          主持人密钥
          <input
            type="password"
            value={key}
            onChange={(e) => setKey(e.target.value)}
            required
            autoComplete="off"
          />
        </label>
        <ErrorNote message={error} />
        <Button className="primary full" disabled={busy}>
          {busy ? "正在验证…" : "进入工作台"}
        </Button>
        <a href={`/rooms/${code}`}>
          以参与者身份加入
          <ArrowRight size={15} />
        </a>
      </form>
    </div>
  );
}

export default function Host({ code }: { code: string }) {
  const { config } = useConfig();
  const [hostKey, setHostKey] = useState("");
  const [accountReady, setAccountReady] = useState(false);
  const [error, setError] = useState("");
  if (config?.yufoloConnected)
    return accountReady ? (
      <RoomPage code={code} hostKey={readHostKey(code)} isHost />
    ) : (
      <div className="center-page">
        <Brand />
        <AccountPanel
          onChange={(user) => {
            if (!user) {
              setAccountReady(false);
              return;
            }
            api(`/rooms/${code}/host`, { key: readHostKey(code) })
              .then(() => setAccountReady(true))
              .catch((e) => setError(e.message));
          }}
        />
        <ErrorNote message={error} />
        <a href={`/rooms/${code}`}>以参与者身份加入</a>
      </div>
    );
  return hostKey ? (
    <RoomPage code={code} hostKey={hostKey} />
  ) : (
    <HostGate code={code} onReady={setHostKey} />
  );
}
