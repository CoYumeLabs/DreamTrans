import type { ReactNode, ButtonHTMLAttributes } from "react";
import { AudioLines, CircleHelp, MessageCircle } from "lucide-react";

export function Brand({ light = false }: { light?: boolean }) {
  return (
    <a
      href="/"
      className={`brand ${light ? "light" : ""}`}
      aria-label="YuAction 首页"
    >
      <span className="brand-symbol">
        <AudioLines size={22} strokeWidth={2.5} />
      </span>
      <span>
        YuAction<span className="brand-dot">.</span>
      </span>
    </a>
  );
}

export function Button({
  children,
  className = "",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button className={`button ${className}`} {...props}>
      {children}
    </button>
  );
}

export function ErrorNote({ message }: { message: string }) {
  return message ? (
    <div className="error-note" role="alert">
      <CircleHelp size={17} />
      {message}
    </div>
  ) : null;
}

export function Pill({
  children,
  tone = "",
}: {
  children: ReactNode;
  tone?: string;
}) {
  return <span className={`pill ${tone}`}>{children}</span>;
}

export function Time({ value }: { value: string }) {
  return (
    <time dateTime={value}>
      {new Date(value).toLocaleTimeString("zh-CN", {
        hour: "2-digit",
        minute: "2-digit",
      })}
    </time>
  );
}

export function Connection({ state }: { state: string }) {
  return (
    <span
      className={`connection ${state === "live" ? "" : "waiting"}`}
      role="status"
    >
      <i />
      {state === "live"
        ? "实时同步中"
        : state === "connecting"
          ? "正在连接"
          : "连接中断，正在重连"}
    </span>
  );
}

export function Empty({
  icon = <MessageCircle size={26} />,
  title,
  children,
}: {
  icon?: ReactNode;
  title: string;
  children: ReactNode;
}) {
  return (
    <div className="empty-state">
      <span className="empty-icon">{icon}</span>
      <strong>{title}</strong>
      <p>{children}</p>
    </div>
  );
}
