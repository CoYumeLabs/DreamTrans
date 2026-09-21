import ReactMarkdown from "react-markdown";
import type { AnswerDraft as Draft, DraftPart } from "../useAssistant";
import { useMessages } from "../i18n";
import { localizeError } from "../i18n/errors";

function Part({ part, title }: { part: DraftPart; title: string }) {
  const m = useMessages();
  return (
    <section className="draft-part">
      <h4>{title}</h4>
      {part.status === "processing" ? (
        <p role="status">{m.draft.generating}</p>
      ) : (
        <>
          {part.text && (
            <div className="markdown-content">
              <ReactMarkdown
                skipHtml
                components={{
                  a: ({ children }) => <span>{children}</span>,
                  img: () => null,
                }}
              >
                {part.text}
              </ReactMarkdown>
            </div>
          )}
          {part.error && (
            <p className="form-note">{localizeError(part.error)}</p>
          )}
          {!!part.sources?.length && (
            <details>
              <summary>{m.draft.sources(part.sources.length)}</summary>
              {part.sources.map((s, i) => (
                <blockquote key={`${s.documentId}-${i}`}>
                  <strong>
                    [{i + 1}] {s.name}
                    {s.chunk ? ` · ${m.draft.chunk(s.chunk)}` : ""}
                  </strong>
                  {s.text && <p>{s.text}</p>}
                </blockquote>
              ))}
            </details>
          )}
        </>
      )}
    </section>
  );
}
export default function AnswerDraft({ draft }: { draft: Draft }) {
  const m = useMessages();
  return (
    <details className="answer-draft" open>
      <summary>{m.draft.summary}</summary>
      <div className="draft-grid">
        <Part title={m.draft.generic} part={draft.generic} />
        <Part title={m.draft.knowledge} part={draft.knowledge} />
      </div>
    </details>
  );
}
