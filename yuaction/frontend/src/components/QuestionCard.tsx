import { AudioLines, Check, Monitor, Sparkles, Trash2 } from "lucide-react";
import type { AnswerDraft as Draft } from "../useAssistant";
import AnswerDraft from "./AnswerDraft";
import type { Question } from "../api";
import { Button, Pill, Time } from "./ui";
import { useMessages } from "../i18n";

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
  const m = useMessages();
  return (
    <article className={`question-card ${q.status}`}>
      <div className="question-meta">
        <span className="question-author">
          <span className="avatar">?</span>
          {m.question.someone}
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
            ? m.question.showing
            : q.status === "answered"
              ? m.question.answered
              : m.question.pending}
        </Pill>
      </div>
      {q.segmentId && (
        <details className="question-quote">
          <summary className="question-context">
            <AudioLines size={13} />
            {m.question.fromCaption}
          </summary>
          <blockquote>{q.quotedText || m.question.quoteMissing}</blockquote>
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
            {draft ? m.question.regenerate : m.question.generate}
          </Button>
          {q.status !== "showing" && (
            <Button
              className="soft small"
              disabled={disabled}
              onClick={() => onStatus("showing")}
            >
              <Monitor size={14} />
              {m.question.show}
            </Button>
          )}
          {q.status !== "answered" && (
            <Button
              className="text small"
              disabled={disabled}
              onClick={() => onStatus("answered")}
            >
              <Check size={15} />
              {m.question.markAnswered}
            </Button>
          )}
          {q.status !== "pending" && (
            <Button
              className="text small"
              disabled={disabled}
              onClick={() => onStatus("pending")}
            >
              {m.question.moveBack}
            </Button>
          )}
          <Button
            className="text small"
            onClick={onDelete}
            aria-label={m.question.deleteLabel(q.content)}
          >
            <Trash2 size={14} />
            {m.question.delete}
          </Button>
        </div>
      )}
      {host && draft && <AnswerDraft draft={draft} />}
    </article>
  );
}
