import { useEffect, useRef, useState } from "react";
import { BookOpen, FileUp, Sparkles, Trash2 } from "lucide-react";
import { api } from "../api";
import type { AssistantInfo, AssistantSettings } from "../useAssistant";
import { Button, ErrorNote, Pill } from "./ui";
import { useMessages, type Messages } from "../i18n";
import { localizeError } from "../i18n/errors";

function documentStatus(status: string, labels: Messages["assistant"]["docStatus"]) {
  return status in labels ? labels[status as keyof typeof labels] : status;
}
type IndexPreview = {
  estimated_dp: number;
  pending_chunks: number;
  requires_indexing: boolean;
  confirmation_token?: string;
};
export default function AssistantPanel({
  code,
  hostKey,
  info,
  error,
  refresh,
}: {
  code: string;
  hostKey: string;
  info: AssistantInfo | null;
  error: string;
  refresh: () => Promise<void>;
}) {
  const m = useMessages();
  const [settings, setSettings] = useState<AssistantSettings | null>(null);
  const [message, setMessage] = useState("");
  const [failure, setFailure] = useState("");
  const [busy, setBusy] = useState(false);
  const [projects, setProjects] = useState<{ id: string; name: string }[]>([]);
  const [preview, setPreview] = useState<IndexPreview | null>(null);
  const [requestId, setRequestId] = useState("");
  const input = useRef<HTMLInputElement>(null);
  const indexDialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    if (info && !settings) setSettings(info.settings);
  }, [info, settings]);
  const base = `/rooms/${code}/assistant`;
  async function act(operation: () => Promise<unknown>, success: string) {
    setBusy(true);
    setFailure("");
    setMessage("");
    try {
      await operation();
      await refresh();
      setMessage(success);
    } catch (e) {
      setFailure((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  async function upload(file: File) {
    if (file.size > 10 * 1024 * 1024) {
      setFailure(m.assistant.fileTooBig);
      return;
    }
    await act(async () => {
      const form = new FormData();
      form.append("file", file);
      const response = await fetch(`/api${base}/documents`, {
        method: "POST",
        headers: hostKey ? { Authorization: `Bearer ${hostKey}` } : {},
        body: form,
        signal: AbortSignal.timeout(60000),
      });
      const body = await response.json();
      if (!response.ok) throw new Error(body.error || m.assistant.uploadFailed);
    }, m.assistant.uploaded);
    setSettings(null);
  }
  async function previewIndex() {
    await act(async () => {
      const next = await api<IndexPreview>(`${base}/index-preview`, {
        method: "POST",
        key: hostKey,
      });
      setPreview(next);
      setRequestId(crypto.randomUUID());
      indexDialog.current?.showModal();
    }, "");
  }
  return (
    <section className="panel assistant-panel" id="assistant">
      <div className="panel-heading">
        <h3>
          <BookOpen size={18} />
          {m.assistant.title}
        </h3>
        <Pill tone="purple">{m.assistant.hostOnly}</Pill>
      </div>
      <p className="form-note">{m.assistant.intro}</p>
      <ErrorNote message={error || failure} />
      {info && !info.configured && (
        <p className="assistant-notice">
          {info.message
            ? localizeError(info.message)
            : m.assistant.notConfigured}
        </p>
      )}
      {info?.provider === "yufolo" && (
        <p className="form-note">
          {m.assistant.yufolo}
        </p>
      )}
      {settings && info && (
        <>
          <details className="assistant-settings">
            <summary>{m.assistant.settings}</summary>
            {info.provider === "yufolo" && (
              <label>
                {m.assistant.project}
                <select
                  aria-label={m.assistant.projectLabel}
                  value={settings.projectId || ""}
                  onFocus={() =>
                    void api<{ projects: { id: string; name: string }[] }>(
                      `${base}/projects`,
                      { key: hostKey },
                    )
                      .then((v) => setProjects(v.projects || []))
                      .catch((e) => setFailure(e.message))
                  }
                  onChange={(e) =>
                    setSettings({ ...settings, projectId: e.target.value })
                  }
                >
                  <option value="">{m.assistant.newProject}</option>
                  {settings.projectId &&
                    !projects.some((p) => p.id === settings.projectId) && (
                      <option value={settings.projectId}>
                        {m.assistant.currentProject}
                      </option>
                    )}
                  {projects.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                </select>
              </label>
            )}
            <label className="check-label">
              <input
                type="checkbox"
                checked={settings.autoAnswer}
                disabled={!info.configured}
                onChange={(e) =>
                  setSettings({ ...settings, autoAnswer: e.target.checked })
                }
              />
              {m.assistant.auto}
            </label>
            <label>
              {m.assistant.genericPrompt}
              <textarea
                aria-label={m.assistant.genericPrompt}
                value={settings.genericPrompt}
                maxLength={2000}
                onChange={(e) =>
                  setSettings({ ...settings, genericPrompt: e.target.value })
                }
              />
            </label>
            <label>
              {m.assistant.kbPrompt}
              <textarea
                aria-label={m.assistant.kbPrompt}
                value={settings.kbPrompt}
                maxLength={2000}
                onChange={(e) =>
                  setSettings({ ...settings, kbPrompt: e.target.value })
                }
              />
            </label>
            <label>
              {m.assistant.topK}
              <input
                aria-label={m.assistant.topK}
                type="number"
                min={1}
                max={8}
                value={settings.topK}
                onChange={(e) =>
                  setSettings({ ...settings, topK: Number(e.target.value) })
                }
              />
            </label>
            <div className="assistant-actions">
              <Button
                className="primary small"
                disabled={busy}
                onClick={() =>
                  void act(
                    () =>
                      api(`${base}/settings`, {
                        method: "PUT",
                        key: hostKey,
                        body: settings,
                      }),
                    m.assistant.saved,
                  )
                }
              >
                {m.assistant.save}
              </Button>
              <Button
                className="text small"
                disabled={busy}
                onClick={() =>
                  setSettings({ ...settings, genericPrompt: "", kbPrompt: "" })
                }
              >
                {m.assistant.reset}
              </Button>
            </div>
          </details>
          <div className="assistant-actions">
            <input
              aria-label={m.assistant.uploadLabel}
              ref={input}
              hidden
              type="file"
              accept={
                info.provider === "yufolo"
                  ? ".pdf,.docx,.txt,.md,.csv,.tsv,.json,.xlsx,.png,.jpg,.jpeg,.webp"
                  : ".pdf,.docx,.txt,.md,.html,.htm,.csv,.json,.xlsx"
              }
              onChange={(e) => {
                const file = e.target.files?.[0];
                if (file) void upload(file);
                e.target.value = "";
              }}
            />
            <Button
              className="soft small"
              disabled={busy || !info.embeddingConfigured}
              onClick={() => input.current?.click()}
            >
              <FileUp size={16} />
              {m.assistant.upload}
            </Button>
            {info.provider === "yufolo" && (
              <Button
                className="outline small"
                disabled={
                  busy || !info.documents.some((d) => d.status === "ready")
                }
                onClick={() => void previewIndex()}
              >
                <Sparkles size={15} />
                {m.assistant.index}
              </Button>
            )}
          </div>
          <p className="form-note">
            {info.provider === "yufolo"
              ? m.assistant.formatsYufolo
              : m.assistant.formatsLocal}{" "}
            · {m.assistant.size}
          </p>
          {info.documents.length === 0 ? (
            <p className="muted">{m.assistant.empty}</p>
          ) : (
            <ul className="document-list">
              {info.documents.map((d) => (
                <li key={d.id}>
                  <div>
                    <strong>{d.name}</strong>
                    <small>
                      {documentStatus(d.status, m.assistant.docStatus)} ·{" "}
                      {m.assistant.chunks(d.chunkCount)}
                      {d.indexStatus
                        ? ` · ${m.assistant.semantic}：${
                            d.indexStatus === "ready"
                              ? m.assistant.ready
                              : d.indexStatus
                          }`
                        : ""}
                    </small>
                    {d.error && (
                      <span className="error-note">{localizeError(d.error)}</span>
                    )}
                  </div>
                  <div className="document-actions">
                    {["failed", "interrupted", "outdated"].includes(
                      d.status,
                    ) && (
                      <Button
                        className="text small"
                        disabled={busy}
                        onClick={() =>
                          void act(
                            () =>
                              api(`${base}/documents/${d.id}/index`, {
                                method: "POST",
                                key: hostKey,
                              }),
                            m.assistant.requeued,
                          )
                        }
                      >
                        {m.assistant.retry}
                      </Button>
                    )}
                    <Button
                      className="text small"
                      aria-label={m.assistant.deleteLabel(d.name)}
                      disabled={busy}
                      onClick={() => {
                        if (
                          confirm(
                            m.assistant.deleteConfirm(
                              d.name,
                              info.provider === "yufolo",
                            ),
                          )
                        )
                          void act(
                            () =>
                              api(`${base}/documents/${d.id}`, {
                                method: "DELETE",
                                key: hostKey,
                              }),
                            m.assistant.deleted,
                          );
                      }}
                    >
                      <Trash2 size={15} />
                    </Button>
                  </div>
                </li>
              ))}
            </ul>
          )}
        </>
      )}
      {message && (
        <p className="success-note" role="status">
          {message}
        </p>
      )}
      <dialog
        ref={indexDialog}
        className="dialog-panel"
        aria-labelledby="index-dialog-title"
      >
        <h2 id="index-dialog-title">{m.assistant.indexTitle}</h2>
        {preview && (
          <>
            <p>
              {preview.requires_indexing
                ? m.assistant.indexPending(
                    preview.pending_chunks,
                    preview.estimated_dp,
                  )
                : m.assistant.indexDone}
            </p>
            <div className="assistant-actions">
              <Button
                className="outline"
                onClick={() => indexDialog.current?.close()}
              >
                {m.assistant.cancel}
              </Button>
              {preview.requires_indexing && (
                <Button
                  className="primary"
                  disabled={busy}
                  onClick={() =>
                    void act(async () => {
                      await api(`${base}/index`, {
                        method: "POST",
                        key: hostKey,
                        body: {
                          confirmed: true,
                          confirmationToken: preview.confirmation_token,
                          requestId,
                        },
                      });
                      indexDialog.current?.close();
                    }, m.assistant.indexSubmitted)
                  }
                >
                  {m.assistant.confirmIndex}
                </Button>
              )}
            </div>
            <ErrorNote message={failure} />
          </>
        )}
      </dialog>
    </section>
  );
}
