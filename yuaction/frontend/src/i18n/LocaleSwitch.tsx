import { LOCALES, useLocale, useMessages } from "./index";

export function LocaleSwitch({ className = "" }: { className?: string }) {
  const [locale, setLocale] = useLocale();
  const m = useMessages();
  return (
    <div
      aria-label={m.locale.switchLabel}
      className={`ya-locale-switch${className ? ` ${className}` : ""}`}
      role="radiogroup"
    >
      {LOCALES.map((item) => (
        <button
          aria-checked={item.code === locale}
          className={item.code === locale ? "is-active" : undefined}
          key={item.code}
          onClick={() => setLocale(item.code)}
          role="radio"
          type="button"
        >
          {item.label}
        </button>
      ))}
    </div>
  );
}
