import { useLayoutEffect, useRef, useState } from "react";
import { ArrowDown, AudioLines, MessageCircle } from "lucide-react";
import type { Segment } from "../api";
import { Empty, Pill, Time } from "./ui";

export default function Captions({
  segments,
  onQuote,
  large = false,
}: {
  segments: Segment[];
  onQuote?: (s: Segment) => void;
  large?: boolean;
}) {
  const [language, setLanguage] = useState("both");
  const list = useRef<HTMLDivElement>(null);
  const [follow, setFollow] = useState(true);
  const latest = segments.at(-1)?.id;
  useLayoutEffect(() => {
    if (follow && list.current)
      list.current.scrollTop = list.current.scrollHeight;
  }, [latest, language, follow]);
  return (
    <section className={`panel caption-panel ${large ? "large-captions" : ""}`}>
      <div className="panel-heading">
        <h3>
          <AudioLines size={18} />
          共享字幕
        </h3>
        <select
          aria-label="字幕显示方式"
          value={language}
          onChange={(e) => setLanguage(e.target.value)}
        >
          <option value="both">双语</option>
          <option value="original">原文</option>
        </select>
      </div>
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
          {segments.slice(-8).map((s) => (
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
              {language === "both" && s.translation && (
                <p className="translation">{s.translation}</p>
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
