import { AudioLines, Check, Monitor, Sparkles, Trash2 } from "lucide-react";
import type { AnswerDraft as Draft } from "../useAssistant";
import AnswerDraft from "./AnswerDraft";
import type { Question } from "../api";
import { Button, Pill, Time } from "./ui";

export default function QuestionCard({
  question: q,
  host,
  disabled,
  onStatus,
  onDelete,
  onGenerate,
  draft,
  aiEnabled,
}: {
  question: Question;
  host: boolean;
  disabled: boolean;
  onStatus: (status: Question["status"]) => void;
  onDelete?: () => void;
  onGenerate?: () => void;
  draft?: Draft;
  aiEnabled?: boolean;
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
          <Button
            className="soft small"
            disabled={
              !aiEnabled ||
              draft?.generic.status === "processing" ||
              draft?.knowledge.status === "processing"
            }
            onClick={onGenerate}
          >
            <Sparkles size={14} />
            {draft ? "重新生成 AI 建议" : "生成 AI 建议"}
          </Button>
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
          <Button
            className="text small"
            onClick={onDelete}
            aria-label={`删除问题：${q.content}`}
          >
            <Trash2 size={14} />
            删除
          </Button>
        </div>
      )}
      {host && draft && <AnswerDraft draft={draft} />}
    </article>
  );
}
