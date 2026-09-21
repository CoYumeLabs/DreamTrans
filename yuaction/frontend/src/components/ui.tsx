import type { ReactNode, ButtonHTMLAttributes } from "react";
import { AudioLines, CircleHelp, MessageCircle } from "lucide-react";
import { intlLocale, useLocale, useMessages } from "../i18n";
import { localizeError } from "../i18n/errors";

export function Brand({ light = false }: { light?: boolean }) {
  const m = useMessages();
  return (
    <a
      href="/"
      className={`brand ${light ? "light" : ""}`}
      aria-label={m.brand.home}
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
  const text = localizeError(message);
  return text ? (
    <div className="error-note" role="alert">
      <CircleHelp size={17} />
      {text}
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
  const [locale] = useLocale();
  return (
    <time dateTime={value}>
      {new Date(value).toLocaleTimeString(intlLocale(locale), {
        hour: "2-digit",
        minute: "2-digit",
      })}
    </time>
  );
}

export function Connection({ state }: { state: string }) {
  const m = useMessages();
  return (
    <span
      className={`connection ${state === "live" ? "" : "waiting"}`}
      role="status"
    >
      <i />
      {state === "live"
        ? m.connection.live
        : state === "connecting"
          ? m.connection.connecting
          : m.connection.interrupted}
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
