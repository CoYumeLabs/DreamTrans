import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { ArrowDown, AudioLines, MessageCircle } from "lucide-react";
import { api, type Segment } from "../api";
import { captionFeed } from "../captionFeed";
import { Empty, Pill, Time } from "./ui";

export default function Captions({
  segments,
  onQuote,
  large = false,
  code = "",
  chooseTranslation = false,
}: {
  segments: Segment[];
  onQuote?: (s: Segment) => void;
  large?: boolean;
  code?: string;
  chooseTranslation?: boolean;
}) {
  const [language, setLanguage] = useState("both");
  const [target, setTarget] = useState(
    () => localStorage.getItem(`yuaction.translation.${code}`) || "",
  );
  const [translationError, setTranslationError] = useState("");
  const captions = useMemo(
    () => captionFeed(segments, chooseTranslation ? target : "").slice(-8),
    [segments, chooseTranslation, target],
  );
  const latestSegments = useRef(segments);
  const visibleIds = useRef<string[]>([]);
  latestSegments.current = segments;
  visibleIds.current = captions.flatMap((caption) => [...caption.segmentIds!]);
  useEffect(() => {
    if (!chooseTranslation || !target || !code) return;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      const pending = latestSegments.current
        .filter((s) => visibleIds.current.includes(s.id))
        .some(
          (s) =>
            s.source === "yufolo" &&
            !s.translations?.[target] &&
            !s.translationErrors?.[target],
        );
      if (pending) {
        try {
          await api(`/rooms/${code}/translations`, {
            method: "POST",
            body: { language: target, segmentIds: visibleIds.current },
          });
          if (active) setTranslationError("");
        } catch (e) {
          if (active) setTranslationError((e as Error).message);
        }
      }
      if (active) timer = setTimeout(() => void poll(), 3000);
    };
    void poll();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [chooseTranslation, target, code]);
  const list = useRef<HTMLDivElement>(null);
  const [follow, setFollow] = useState(true);
  const latest = `${captions.at(-1)?.id}:${captions.at(-1)?.text}:${captions.at(-1)?.translation}`;
  useLayoutEffect(() => {
    if (follow && list.current)
      list.current.scrollTop = list.current.scrollHeight;
  }, [latest, language, follow]);
  return (
    <section
      id="captions"
      className={`panel caption-panel ${large ? "large-captions" : ""}`}
    >
      <div className="panel-heading">
        <h3>
          <AudioLines size={18} />
          共享字幕
        </h3>
        {chooseTranslation ? (
          <select
            aria-label="我的译文语言"
            value={target}
            onChange={(e) => {
              setTarget(e.target.value);
              setTranslationError("");
              localStorage.setItem(
                `yuaction.translation.${code}`,
                e.target.value,
              );
            }}
          >
            <option value="">只看原文</option>
            {[
              ["cmn", "中文"],
              ["en", "English"],
              ["ja", "日本語"],
              ["ko", "한국어"],
              ["de", "Deutsch"],
              ["fr", "Français"],
              ["es", "Español"],
            ].map(([v, label]) => (
              <option key={v} value={v}>
                {label}
              </option>
            ))}
          </select>
        ) : segments.some((s) => s.translation) ? (
          <select
            aria-label="字幕显示方式"
            value={language}
            onChange={(e) => setLanguage(e.target.value)}
          >
            <option value="both">双语</option>
            <option value="original">原文</option>
          </select>
        ) : null}
      </div>
      {translationError && (
        <p className="form-note" role="status">
          {translationError}
        </p>
      )}
      {segments.length ? (
        <div
          className="caption-list"
          ref={list}
          onScroll={() => {
            const el = list.current;
            if (el)
              setFollow(el.scrollHeight - el.scrollTop - el.clientHeight < 35);
          }}
        >
          {captions.map((s) => (
            <article className="caption" key={s.id}>
              <div className="caption-meta">
                <Time value={s.createdAt} />
                {s.source === "demo" && <Pill tone="neutral">演示字幕</Pill>}
                {onQuote && (
                  <button
                    onClick={() => onQuote(s)}
                    aria-label={`针对字幕提问：${s.text}`}
                  >
                    <MessageCircle size={14} />
                    针对这段提问
                  </button>
                )}
              </div>
              <p>{s.text}</p>
              {chooseTranslation && target ? (
                <>
                  {s.translations?.[target] ? (
                    <p className="translation">{s.translations[target]}</p>
                  ) : null}
                  {s.translationErrors?.[target] ? (
                    <p className="form-note">
                      {s.translationErrors[target]}{" "}
                      <button
                        onClick={() =>
                          void api(`/rooms/${code}/translations`, {
                            method: "POST",
                            body: {
                              language: target,
                              retry: true,
                              segmentIds: s.segmentIds,
                            },
                          }).catch((e) => setTranslationError(e.message))
                        }
                      >
                        重试译文
                      </button>
                    </p>
                  ) : s.translationPending ? (
                    <p className="form-note">等待句段完成并翻译…</p>
                  ) : null}
                </>
              ) : (
                language === "both" &&
                s.translation && <p className="translation">{s.translation}</p>
              )}
            </article>
          ))}
        </div>
      ) : (
        <Empty icon={<AudioLines size={27} />} title="等待共享字幕">
          <span>主持人连接转录后，大家将在这里同步看到内容。</span>
        </Empty>
      )}
      {!follow && (
        <button className="caption-follow" onClick={() => setFollow(true)}>
          <ArrowDown size={14} />
          回到最新字幕
        </button>
      )}
    </section>
  );
}
