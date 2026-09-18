import { useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import { Button, ErrorNote } from "./ui";

export type Account = { id: string; name: string; email: string };
export default function AccountPanel({
  onChange,
}: {
  onChange: (user: Account | null) => void;
}) {
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
    <section className="panel account-panel">
      {user ? (
        <>
          <h3>{user.name || user.email}</h3>
          <p>已连接 Yufolo · 活动归属于此账号</p>
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
            退出登录
          </Button>
        </>
      ) : (
        <form onSubmit={login}>
          <h3>使用 Yufolo 账号登录</h3>
          <p>老师登录后创建活动；观众扫码即可参与。</p>
          <label>
            邮箱
            <input
              type="email"
              autoComplete="username"
              required
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          </label>
          <label>
            密码
            <input
              type="password"
              autoComplete="current-password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </label>
          <Button className="primary" disabled={busy}>
            {busy ? "正在登录…" : "登录 Yufolo"}
          </Button>
        </form>
      )}
      <ErrorNote message={error} />
    </section>
  );
}
