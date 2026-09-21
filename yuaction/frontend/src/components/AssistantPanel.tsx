import { useEffect, useRef, useState } from "react";
import { BookOpen, FileUp, Sparkles, Trash2 } from "lucide-react";
import { api } from "../api";
import type { AssistantInfo, AssistantSettings } from "../useAssistant";
import { Button, ErrorNote, Pill } from "./ui";

const statuses: Record<string, string> = {
  ready: "可检索",
  pending: "等待提取",
  processing: "处理中",
  extracting: "提取中",
  indexing: "索引中",
  failed: "处理失败",
  interrupted: "处理中断",
  outdated: "需要重新索引",
  queued: "排队中",
};
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
      setFailure("单个文件最多 10 MB");
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
      if (!response.ok) throw new Error(body.error || "上传失败");
    }, "资料已上传，处理状态会自动更新。");
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
          资料与 AI
        </h3>
        <Pill tone="purple">主持人专用</Pill>
      </div>
      <p className="form-note">
        上传讲义，为现场问题生成通用建议和有资料依据的回答。AI
        草稿仅主持人可见。
      </p>
      <ErrorNote message={error || failure} />
      {info && !info.configured && (
        <p className="assistant-notice">
          {info.message || "尚未配置 AI。请在服务端设置聊天接口、密钥与模型。"}
        </p>
      )}
      {info?.provider === "yufolo" && (
        <p className="form-note">
          资料、模型和额度由 Yufolo 管理。生成回答和语义索引使用你的 Yufolo
          余额。
        </p>
      )}
      {settings && info && (
        <>
          <details className="assistant-settings">
            <summary>知识库与回答设置</summary>
            {info.provider === "yufolo" && (
              <label>
                关联 Yufolo 知识库
                <select
                  aria-label="关联 Yufolo 知识库"
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
                  <option value="">为这个活动创建知识库</option>
                  {settings.projectId &&
                    !projects.some((p) => p.id === settings.projectId) && (
                      <option value={settings.projectId}>当前关联知识库</option>
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
              收到新问题时自动生成 AI 建议
            </label>
            <label>
              通用回答提示词
              <textarea
                aria-label="通用回答提示词"
                value={settings.genericPrompt}
                maxLength={2000}
                onChange={(e) =>
                  setSettings({ ...settings, genericPrompt: e.target.value })
                }
              />
            </label>
            <label>
              知识库回答提示词
              <textarea
                aria-label="知识库回答提示词"
                value={settings.kbPrompt}
                maxLength={2000}
                onChange={(e) =>
                  setSettings({ ...settings, kbPrompt: e.target.value })
                }
              />
            </label>
            <label>
              检索片段数量
              <input
                aria-label="检索片段数量"
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
                    "设置已保存。",
                  )
                }
              >
                保存回答设置
              </Button>
              <Button
                className="text small"
                disabled={busy}
                onClick={() =>
                  setSettings({ ...settings, genericPrompt: "", kbPrompt: "" })
                }
              >
                恢复默认提示词
              </Button>
            </div>
          </details>
          <div className="assistant-actions">
            <input
              aria-label="上传知识库资料"
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
              上传资料
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
                建立语义索引
              </Button>
            )}
          </div>
          <p className="form-note">
            {info.provider === "yufolo"
              ? "PDF、DOCX、TXT、Markdown、CSV、TSV、JSON、XLSX、图片"
              : "PDF、DOCX、TXT、Markdown、HTML、CSV、JSON、XLSX"}{" "}
            · 单个文件最多 10 MB
          </p>
          {info.documents.length === 0 ? (
            <p className="muted">还没有资料。也可以先使用通用 AI 建议。</p>
          ) : (
            <ul className="document-list">
              {info.documents.map((d) => (
                <li key={d.id}>
                  <div>
                    <strong>{d.name}</strong>
                    <small>
                      {statuses[d.status] || d.status} · {d.chunkCount} 个片段
                      {d.indexStatus
                        ? ` · 语义索引：${d.indexStatus === "ready" ? "就绪" : d.indexStatus}`
                        : ""}
                    </small>
                    {d.error && <span className="error-note">{d.error}</span>}
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
                            "已重新提交处理。",
                          )
                        }
                      >
                        重试
                      </Button>
                    )}
                    <Button
                      className="text small"
                      aria-label={`删除资料 ${d.name}`}
                      disabled={busy}
                      onClick={() => {
                        if (
                          confirm(
                            `删除资料“${d.name}”？${info.provider === "yufolo" ? "这也会从关联的 Yufolo 知识库删除。" : ""}`,
                          )
                        )
                          void act(
                            () =>
                              api(`${base}/documents/${d.id}`, {
                                method: "DELETE",
                                key: hostKey,
                              }),
                            "资料已删除。",
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
        <h2 id="index-dialog-title">建立知识库语义索引</h2>
        {preview && (
          <>
            <p>
              {preview.requires_indexing
                ? `将处理 ${preview.pending_chunks} 个片段，Yufolo 预计扣除 ${preview.estimated_dp} DP。`
                : "当前资料已经完成索引。"}
            </p>
            <div className="assistant-actions">
              <Button
                className="outline"
                onClick={() => indexDialog.current?.close()}
              >
                取消
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
                    }, "语义索引已提交，完成后将用于资料检索。")
                  }
                >
                  确认并建立索引
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
