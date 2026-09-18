import { AudioLines, Check, Monitor } from "lucide-react";
import type { Question } from "../api";
import { Button, Pill, Time } from "./ui";

export default function QuestionCard({
  question: q,
  host,
  disabled,
  onStatus,
}: {
  question: Question;
  host: boolean;
  disabled: boolean;
  onStatus: (status: Question["status"]) => void;
}) {
  return (
    <article className={`question-card ${q.status}`}>
      <div className="question-meta">
        <span className="question-author">
          <span className="avatar">?</span>一位参与者
          <Time value={q.createdAt} />
        </span>
        <Pill
          tone={
            q.status === "showing"
              ? "purple"
              : q.status === "answered"
                ? "green"
                : "neutral"
          }
        >
          {q.status === "showing"
            ? "正在展示"
            : q.status === "answered"
              ? "已解答"
              : "待回应"}
        </Pill>
      </div>
      {q.segmentId && (
        <details className="question-quote">
          <summary className="question-context">
            <AudioLines size={13} />
            来自一段共享字幕 · 查看原文
          </summary>
          <blockquote>{q.quotedText || "引用字幕暂不可用"}</blockquote>
        </details>
      )}
      <p>{q.content}</p>
      {host && (
        <div className="question-actions">
          {q.status !== "showing" && (
            <Button
              className="soft small"
              disabled={disabled}
              onClick={() => onStatus("showing")}
            >
              <Monitor size={14} />
              展示问题
            </Button>
          )}
          {q.status !== "answered" && (
            <Button
              className="text small"
              disabled={disabled}
              onClick={() => onStatus("answered")}
            >
              <Check size={15} />
              标记已解答
            </Button>
          )}
          {q.status !== "pending" && (
            <Button
              className="text small"
              disabled={disabled}
              onClick={() => onStatus("pending")}
            >
              移回待回应
            </Button>
          )}
        </div>
      )}
    </article>
  );
}
