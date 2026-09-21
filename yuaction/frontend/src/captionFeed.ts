import { TranscriptFeedModel } from "../../../frontend/src/core/transcription/TranscriptFeedModel";
import { normalizeTranscriptText } from "../../../frontend/src/core/transcription/scriptText";
import type { Segment } from "./api";

export type Caption = Segment & { translationPending: boolean };

// Room snapshots may contain old atomic finals, externally ingested fragments,
// or already aggregated records. Use Yufolo's actual presentation model for all
// of them; never rewrite the stored identities used by questions and translations.
export function captionFeed(segments: Segment[], target = ""): Caption[] {
  const captions: Caption[] = [];
  let batch: Segment[] = [];
  const flush = () => {
    if (!batch.length) return;
    const byId = new Map(batch.map((segment) => [segment.id, segment]));
    const origin = Date.parse(batch[0].createdAt);
    const timing = (segment: Segment) => {
      const fallback = Math.max(
        0,
        (Date.parse(segment.createdAt) - origin) / 1000,
      );
      return {
        startTime:
          segment.startTime ?? (segment.endTime !== undefined ? 0 : fallback),
        endTime: segment.endTime ?? segment.startTime ?? fallback,
      };
    };
    const model = new TranscriptFeedModel({
      sourceLanguage: "",
      targetLanguage: target,
      translationEnabled: true,
    });
    model.hydrate(
      batch.map((segment, sequence) => ({
        id: segment.id,
        sequence,
        speaker: segment.speaker || "Speaker",
        text: normalizeTranscriptText(segment.text),
        status: "final" as const,
        receivedAt: Date.parse(segment.createdAt),
        source: segment.source,
        ...timing(segment),
      })),
      batch.flatMap((segment, sequence) => {
        const text = target
          ? segment.translations?.[target]
          : segment.translation;
        return text === undefined
          ? []
          : [
              {
                id: `translation:${segment.id}:${target}`,
                segmentId: segment.id,
                sequence,
                speaker: segment.speaker || "Speaker",
                language: target,
                text: normalizeTranscriptText(text),
                status: "final" as const,
                receivedAt: Date.parse(segment.updatedAt || segment.createdAt),
                source: segment.source,
                ...timing(segment),
              },
            ];
      }),
    );
    for (const item of model.getSnapshot().items) {
      const ids = item.segmentIds!;
      const first = byId.get(ids[0])!;
      const errors = ids
        .map((id) => byId.get(id)?.translationErrors?.[target])
        .filter(Boolean);
      captions.push({
        ...first,
        id: item.id,
        segmentIds: ids,
        text: item.original?.text || "",
        startTime: item.startTime,
        endTime: item.endTime,
        translation: item.translation?.text,
        translations:
          target && item.translation?.text
            ? { [target]: item.translation.text }
            : {},
        translationErrors:
          target && errors.length ? { [target]: errors[0]! } : {},
        // YuAction translations cover exactly their stored source record. The
        // shared model also supports Yufolo range translations, whose time
        // tolerance must not mark a neighbouring untranslated record complete.
        translationPending: ids.some((id) => {
          const source = byId.get(id)!;
          return !(target ? source.translations?.[target] : source.translation);
        }),
      });
    }
    batch = [];
  };
  for (const segment of segments) {
    if (batch.length && batch[0].source !== segment.source) flush();
    batch.push(segment);
  }
  flush();
  return captions;
}
