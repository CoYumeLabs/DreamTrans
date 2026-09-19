import { useCallback, useEffect, useState } from "react";
import { api } from "./api";

export type DraftPart = {
  status: string;
  text?: string;
  error?: string;
  sources?: {
    documentId: string;
    name: string;
    text?: string;
    chunk?: number;
  }[];
};
export type AnswerDraft = {
  questionId: string;
  generic: DraftPart;
  knowledge: DraftPart;
  updatedAt: string;
};
export type AssistantSettings = {
  autoAnswer: boolean;
  genericPrompt: string;
  kbPrompt: string;
  topK: number;
  projectId: string;
};
export type KnowledgeDocument = {
  id: string;
  name: string;
  status: string;
  error?: string;
  chunkCount: number;
  indexStatus?: string;
  text?: string;
};
export type AssistantInfo = {
  provider?: string;
  configured: boolean;
  embeddingConfigured: boolean;
  message?: string;
  settings: AssistantSettings;
  documents: KnowledgeDocument[];
  answers: AnswerDraft[];
};
export function useAssistant(code: string, key: string, enabled: boolean) {
  const [info, setInfo] = useState<AssistantInfo | null>(null);
  const [error, setError] = useState("");
  const refresh = useCallback(async () => {
    if (!enabled) return;
    const next = await api<AssistantInfo>(`/rooms/${code}/assistant`, { key });
    setInfo(next);
    setError("");
  }, [code, key, enabled]);
  useEffect(() => {
    if (!enabled) return;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      try {
        const next = await api<AssistantInfo>(`/rooms/${code}/assistant`, {
          key,
        });
        if (active) {
          setInfo(next);
          setError("");
        }
      } catch (e) {
        if (active) setError((e as Error).message);
      }
      if (active) timer = setTimeout(() => void poll(), 3000);
    };
    void poll();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [code, key, enabled]);
  async function generate(id: string) {
    setError("");
    try {
      await api(`/rooms/${code}/assistant/answers/${id}`, {
        method: "POST",
        key,
      });
      await refresh();
    } catch (e) {
      setError((e as Error).message);
    }
  }
  return { info, error, refresh, generate };
}
