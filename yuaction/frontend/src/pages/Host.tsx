import { useEffect, useState, type FormEvent } from "react";
import { ArrowRight, Settings2 } from "lucide-react";
import { api, readHostKey, saveHostKey } from "../api";
import { Brand, Button, ErrorNote } from "../components/ui";
import RoomPage from "./RoomPage";
import { useConfig } from "../hooks/useConfig";
import AccountPanel from "../components/AccountPanel";
import { useMessages } from "../i18n";
import { LocaleSwitch } from "../i18n/LocaleSwitch";

function HostGate({
  code,
  onReady,
}: {
  code: string;
  onReady: (key: string) => void;
}) {
  const m = useMessages();
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
      <LocaleSwitch />
      <Brand />
      <form className="panel gate-form" onSubmit={verify}>
        <span className="empty-icon">
          <Settings2 size={26} />
        </span>
        <h1>{m.hostGate.title}</h1>
        <p>{m.hostGate.body(code)}</p>
        <label>
          {m.hostGate.key}
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
          {busy ? m.hostGate.verifying : m.hostGate.enter}
        </Button>
        <a href={`/rooms/${code}`}>
          {m.hostGate.join}
          <ArrowRight size={15} />
        </a>
      </form>
    </div>
  );
}

export default function Host({ code }: { code: string }) {
  const m = useMessages();
  const { config } = useConfig();
  const [hostKey, setHostKey] = useState("");
  const [accountReady, setAccountReady] = useState(false);
  const [error, setError] = useState("");
  if (config?.yufoloConnected)
    return accountReady ? (
      <RoomPage code={code} hostKey={readHostKey(code)} isHost />
    ) : (
      <div className="center-page">
        <LocaleSwitch />
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
        <a href={`/rooms/${code}`}>{m.hostGate.join}</a>
      </div>
    );
  return hostKey ? (
    <RoomPage code={code} hostKey={hostKey} />
  ) : (
    <HostGate code={code} onReady={setHostKey} />
  );
}
