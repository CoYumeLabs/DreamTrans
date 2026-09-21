import ReactMarkdown from "react-markdown";
import type { AnswerDraft as Draft, DraftPart } from "../useAssistant";

function Part({ part, title }: { part: DraftPart; title: string }) {
  return (
    <section className="draft-part">
      <h4>{title}</h4>
      {part.status === "processing" ? (
        <p role="status">正在生成…</p>
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
          {part.error && <p className="form-note">{part.error}</p>}
          {!!part.sources?.length && (
            <details>
              <summary>查看资料依据（{part.sources.length}）</summary>
              {part.sources.map((s, i) => (
                <blockquote key={`${s.documentId}-${i}`}>
                  <strong>
                    [{i + 1}] {s.name}
                    {s.chunk ? ` · 片段 ${s.chunk}` : ""}
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
  return (
    <details className="answer-draft" open>
      <summary>AI 回答参考 · 仅主持人可见</summary>
      <div className="draft-grid">
        <Part title="通用建议" part={draft.generic} />
        <Part title="知识库回答" part={draft.knowledge} />
      </div>
    </details>
  );
}
