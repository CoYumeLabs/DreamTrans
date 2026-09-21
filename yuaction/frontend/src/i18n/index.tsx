import { useSyncExternalStore } from "react";
import { en } from "./en";
import { zhCN } from "./zh-CN";

export type Locale = "zh-CN" | "en";
export type Messages = typeof zhCN;

export const LOCALES: ReadonlyArray<{ code: Locale; label: string; intl: string }> = [
  { code: "zh-CN", label: "中文", intl: "zh-CN" },
  { code: "en", label: "English", intl: "en" },
];

const STORAGE_KEY = "ya_locale";
const DICTIONARIES: Record<Locale, Messages> = { "zh-CN": zhCN, en };

function isLocale(value: unknown): value is Locale {
  return value === "zh-CN" || value === "en";
}

/** Saved choice, then the browser language. Chinese variants map to zh-CN. */
export function detectLocale(): Locale {
  try {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (isLocale(stored)) return stored;
  } catch {
    // Storage unavailable: fall through to the browser language.
  }
  // Only a browser language preference counts. Node also exposes `navigator`,
  // which would flip verification scripts away from the zh-CN dictionary.
  const languages =
    typeof window === "undefined" || typeof navigator === "undefined"
      ? []
      : [...(navigator.languages ?? []), navigator.language];
  for (const language of languages) {
    if (!language) continue;
    if (language.toLowerCase().startsWith("zh")) return "zh-CN";
    if (language.toLowerCase().startsWith("en")) return "en";
  }
  return "zh-CN";
}

let current: Locale = detectLocale();
const listeners = new Set<() => void>();

function applyDocumentLanguage(locale: Locale) {
  if (typeof document === "undefined") return;
  const messages = DICTIONARIES[locale];
  document.documentElement.lang = locale;
  document.title = messages.meta.title;
  document
    .querySelector('meta[name="description"]')
    ?.setAttribute("content", messages.meta.description);
}
applyDocumentLanguage(current);

export function getLocale(): Locale {
  return current;
}

export function setLocale(locale: Locale): void {
  if (locale === current) return;
  current = locale;
  try {
    localStorage.setItem(STORAGE_KEY, locale);
  } catch {
    // Applies to this tab only.
  }
  applyDocumentLanguage(locale);
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function messages(): Messages {
  return DICTIONARIES[current];
}

export function intlLocale(locale: Locale = current): string {
  return LOCALES.find((item) => item.code === locale)?.intl ?? "en";
}

export function useLocale(): [Locale, (locale: Locale) => void] {
  const locale = useSyncExternalStore(subscribe, getLocale, getLocale);
  return [locale, setLocale];
}

export function useMessages(): Messages {
  const locale = useSyncExternalStore(subscribe, getLocale, getLocale);
  return DICTIONARIES[locale];
}
