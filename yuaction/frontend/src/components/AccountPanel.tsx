import { useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import { Button, ErrorNote } from "./ui";
import { ArrowUpRight, AudioLines, LogOut, ShieldCheck } from "lucide-react";

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
    <section className={`panel account-panel ${user ? "signed-in" : ""}`}>
      {user ? (
        <>
          <div className="account-heading">
            <span className="account-symbol">
              <ShieldCheck size={22} />
            </span>
            <span className="eyebrow">你的工作空间</span>
          </div>
          <h3>{user.name || user.email}</h3>
          <p>已连接 Yufolo · 活动归属于此账号</p>
          <div className="account-benefit">
            <AudioLines size={18} />
            <span>
              从现场到课后
              <br />
              <small>转录记录同步保存在 Yufolo</small>
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
            退出登录
          </Button>
        </>
      ) : (
        <form onSubmit={login}>
          <div className="account-heading">
            <span className="account-symbol">
              <AudioLines size={21} />
            </span>
            <div>
              <h3>准备好开讲了吗？</h3>
              <p>使用 Yufolo 账号登录</p>
            </div>
          </div>
          <label>
            邮箱
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
            密码
            <input
              type="password"
              autoComplete="current-password"
              placeholder="输入你的密码"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </label>
          <Button className="primary" disabled={busy}>
            {busy ? "正在登录…" : "登录 Yufolo"}
            <ArrowUpRight size={16} />
          </Button>
          <p className="account-hint">
            <ShieldCheck size={13} /> 听众无需登录，扫码即可参与
          </p>
        </form>
      )}
      <ErrorNote message={error} />
    </section>
  );
}
