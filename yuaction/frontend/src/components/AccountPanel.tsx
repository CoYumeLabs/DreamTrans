import { useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import { Button, ErrorNote } from "./ui";
import { ArrowUpRight, AudioLines, LogOut, ShieldCheck } from "lucide-react";
import { useMessages } from "../i18n";

export type Account = { id: string; name: string; email: string };
export default function AccountPanel({
  onChange,
}: {
  onChange: (user: Account | null) => void;
}) {
  const m = useMessages();
  const [user, setUser] = useState<Account | null>(null);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    let alive = true;
    api<Account>("/auth/me")
      .then((u) => {
        if (alive) {
          setUser(u);
          onChange(u);
        }
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, []);
  async function login(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const u = await api<Account>("/auth/login", {
        method: "POST",
        body: { email, password },
      });
      setPassword("");
      setUser(u);
      onChange(u);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className={`panel account-panel ${user ? "signed-in" : ""}`}>
      {user ? (
        <>
          <div className="account-heading">
            <span className="account-symbol">
              <ShieldCheck size={22} />
            </span>
            <span className="eyebrow">{m.account.workspace}</span>
          </div>
          <h3>{user.name || user.email}</h3>
          <p>{m.account.connected}</p>
          <div className="account-benefit">
            <AudioLines size={18} />
            <span>
              {m.account.benefit}
              <br />
              <small>{m.account.benefitMeta}</small>
            </span>
          </div>
          <Button
            className="outline"
            onClick={async () => {
              try {
                await api("/auth/logout", { method: "POST" });
                setUser(null);
                onChange(null);
              } catch (e) {
                setError((e as Error).message);
              }
            }}
          >
            <LogOut size={15} />
            {m.account.logout}
          </Button>
        </>
      ) : (
        <form onSubmit={login}>
          <div className="account-heading">
            <span className="account-symbol">
              <AudioLines size={21} />
            </span>
            <div>
              <h3>{m.account.ready}</h3>
              <p>{m.account.loginHint}</p>
            </div>
          </div>
          <label>
            {m.account.email}
            <input
              type="email"
              autoComplete="username"
              placeholder="you@example.com"
              required
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          </label>
          <label>
            {m.account.password}
            <input
              type="password"
              autoComplete="current-password"
              placeholder={m.account.passwordPlaceholder}
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </label>
          <Button className="primary" disabled={busy}>
            {busy ? m.account.signingIn : m.account.login}
            <ArrowUpRight size={16} />
          </Button>
          <p className="account-hint">
            <ShieldCheck size={13} /> {m.account.audience}
          </p>
        </form>
      )}
      <ErrorNote message={error} />
    </section>
  );
}
