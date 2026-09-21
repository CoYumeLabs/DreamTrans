import { getLocale } from "./index";

/** Server and stored client messages are Chinese. English UI maps the known ones. */
const enErrors: Record<string, string> = {
  "不允许跨站操作": "Cross-site action is not allowed.",
  "操作较频繁，请稍后再试": "Too many actions. Try again in a moment.",
  "找不到这个房间，请检查房间码": "That room was not found. Check the room code.",
  "房间正在更新，请重试": "The room is being updated. Try again.",
  "服务暂时不可用，请稍后重试": "The service is temporarily unavailable. Try again later.",
  "服务暂时不可用": "The service is temporarily unavailable.",
  "请使用 JSON 请求": "Send a JSON request.",
  "请求内容不正确或过长": "The request is invalid or too long.",
  "请求只能包含一个 JSON 对象": "The request can contain only one JSON object.",
  "数据库暂时不可用": "The database is temporarily unavailable.",
  "请输入正确的活动创建密钥": "Enter the correct activity creation key.",
  "尚未配置活动创建权限": "Activity creation is not configured.",
  "请输入活动名称，并选择课堂或演讲": "Enter an activity name and choose class or talk.",
  "需要此房间的主持人密钥": "This room’s host key is required.",
  "问题需要 1–1000 个字符": "A question needs 1–1000 characters.",
  "活动已结束，暂不接收新问题": "The activity has ended and is not taking new questions.",
  "此预览版每个房间最多接收 300 个问题": "This preview accepts at most 300 questions per room.",
  "引用的字幕无效": "The quoted caption is invalid.",
  "引用的字幕不存在或重复": "The quoted caption does not exist or is duplicated.",
  "问题状态不正确": "That question status is not valid.",
  "请先重新开启活动": "Reopen the activity first.",
  "活动状态不正确": "That activity status is not valid.",
  "需要此房间的主持权限": "Host access to this room is required.",
  "演示字幕未启用": "Demo captions are not enabled.",
  "字幕接入凭证无效": "The caption credential is invalid.",
  "字幕 ID 或内容不正确": "The caption ID or text is invalid.",
  "字幕 ID 已存在且内容不同": "That caption ID already exists with different text.",
  "活动已结束，不接收新字幕": "The activity has ended and is not taking new captions.",
  "此预览版已达到字幕容量上限": "This preview has reached its caption capacity.",
  "此预览版房间连接已满": "This preview room is full.",
  "无法建立实时连接": "The live connection could not be opened.",
  "请求失败，请稍后重试": "The request failed. Try again later.",
  "此活动正在更新，请稍后重试": "This activity is being updated. Try again later.",
  "请先登录 Yufolo 账号": "Sign in to Yufolo first.",
  "请选择原文和不同的翻译语言，或关闭翻译":
    "Choose a source language and a different translation language, or turn translation off.",
  "只有此活动的主持人可以开启转录": "Only this activity’s host can start transcription.",
  "已关联会话的语言不能更改，请创建新活动":
    "The language of a linked session cannot change. Create a new activity.",
  "Yufolo 返回了不匹配的会话": "Yufolo returned a session that does not match.",
  "需要此活动的主持权限": "Host access to this activity is required.",
  "请登录 Yufolo 后结束转录": "Sign in to Yufolo before ending transcription.",
  "转录正在停止，请稍后重试": "Transcription is stopping. Try again later.",
  "需要音频 WebSocket 连接": "An audio WebSocket connection is required.",
  "请先登录 Yufolo": "Sign in to Yufolo first.",
  "只有会话所有者可以采集音频": "Only the session owner can capture audio.",
  "请先关联一个活动中的 Yufolo 会话": "Link a Yufolo session for this activity first.",
  "音频采样率不正确": "The audio sample rate is not valid.",
  "此活动已有主持端正在转录": "A host device is already transcribing this activity.",
  "活动状态已变化，请刷新后重试": "The activity status changed. Refresh and try again.",
  "Yufolo 暂时无法开启转录，请检查余额和并发限制":
    "Yufolo could not start transcription. Check the balance and concurrency limit.",
  "账号连接已失效，请重新登录": "The account connection expired. Sign in again.",
  "转录连接已断开，请重新开始；已确认字幕已保留":
    "The transcription connection closed. Start again. Confirmed captions were kept.",
  "活动已结束": "The activity has ended.",
  "活动字幕已达到容量上限": "This activity has reached its caption capacity.",
  "译文等待队列已满": "The translation queue is full.",
  "字幕同步中断，已收到的原文保留在房间；重新开始会补存到 Yufolo":
    "Caption sync was interrupted. Original text already received stays in the room. Starting again saves the rest to Yufolo.",
  "暂时无法连接 Yufolo，请稍后重试": "Yufolo is unreachable. Try again later.",
  "Yufolo 响应读取失败": "The Yufolo response could not be read.",
  "Yufolo 响应格式不正确": "The Yufolo response was not in the expected format.",
  "请重新登录 Yufolo": "Sign in to Yufolo again.",
  "Yufolo 登录响应不正确": "The Yufolo sign-in response was not valid.",
  "账号状态已变化，请重新登录": "The account status changed. Sign in again.",
  "尚未配置 Yufolo 连接": "The Yufolo connection is not configured.",
  "请输入邮箱和密码": "Enter an email and password.",
  "Yufolo 登录响应不完整": "The Yufolo sign-in response was incomplete.",
  "登录连接已满，请稍后重试": "Sign-in capacity is full. Try again later.",
  "只有此活动的主持人可以访问资料与 AI 草稿":
    "Only this activity’s host can open materials and AI drafts.",
  "检索数量应为 1–8，提示词最多各 2000 字":
    "Retrieval count must be 1–8, and each prompt can be at most 2000 characters.",
  "请先配置知识库向量接口、模型和密钥":
    "Configure the knowledge-base embedding endpoint, model, and key first.",
  "上传失败：单个文件最多 10 MB": "Upload failed: each file can be at most 10 MB.",
  "请选择要上传的资料": "Choose a material to upload.",
  "单个文件最多 10 MB": "Each file can be at most 10 MB.",
  "文件名过长或无效": "The file name is too long or invalid.",
  "每个活动最多 20 份资料，请先删除不需要的资料":
    "Each activity can hold 20 materials. Delete one that is no longer needed.",
  "此资料正在索引": "This material is being indexed.",
  "AI 正忙，请稍后重试": "AI is busy. Try again later.",
  "请先配置知识库向量模型": "Configure the knowledge-base embedding model first.",
  "处理被中断，请重新索引": "Processing was interrupted. Index again.",
  "向量模型已更改，请重新索引": "The embedding model changed. Index again.",
  "生成被中断，请重试": "Generation was interrupted. Try again.",
  "此活动生成较频繁，请稍后再试": "This activity is generating too often. Try again later.",
  "请先配置 AI 聊天接口、模型和密钥": "Configure the AI chat endpoint, model, and key first.",
  "此问题正在生成，请稍候": "This question is still being generated.",
  "请选择支持的译文语言": "Choose a supported translation language.",
  "请求的字幕过多": "Too many captions were requested.",
  "主持人连接 Yufolo 转录后可选择译文":
    "Translation can be chosen after the host connects Yufolo transcription.",
  "主持人需要登录并保持工作台在线，才能生成新译文":
    "The host needs to stay signed in with the workspace open before a new translation can be generated.",
  "译文暂不可用，请稍后重试": "The translation is unavailable. Try again later.",
  "知识库编号无效": "The knowledge-base ID is invalid.",
  "Yufolo 知识库响应无效": "The Yufolo knowledge-base response was invalid.",
  "生成中断，请重新登录后重试": "Generation was interrupted. Sign in again and retry.",
  "Yufolo 尚未启用 AI 服务": "Yufolo does not have AI enabled.",
  "请更新 Yufolo，以启用独立问答并避免不同问题串入历史回答":
    "Update Yufolo so each question is answered on its own and older answers are not mixed in.",
  "请选择不超过 10 MB 的资料": "Choose a material of at most 10 MB.",
  "请选择资料文件": "Choose a material file.",
  "单个资料最多 10 MB": "Each material can be at most 10 MB.",
  "文件名无效": "The file name is invalid.",
  "资料编号无效": "The material ID is invalid.",
  "请在 Yufolo 知识库中查看原始资料": "Open the original material in the Yufolo knowledge base.",
  "独立模式会在上传后自动建立索引": "Standalone mode indexes materials automatically after upload.",
  "请先上传资料或关联知识库": "Upload a material or link a knowledge base first.",
  "此操作需要 Yufolo": "This action requires Yufolo.",
  "请确认索引费用后继续": "Confirm the indexing cost before continuing.",
  "请先关联知识库": "Link a knowledge base first.",
  "主持人需要先登录 Yufolo 并打开工作台":
    "The host needs to sign in to Yufolo and open the workspace first.",
  "此问题正在生成": "This question is still being generated.",
  "活动所属账号已变化，请重新登录": "The activity’s account changed. Sign in again.",
  "请先更新 Yufolo，以支持独立问答": "Update Yufolo before using separate answers.",
  "上传资料或关联 Yufolo 知识库后，可生成资料回答":
    "Upload a material or link a Yufolo knowledge base to generate a sourced answer.",
  "Yufolo 未返回回答内容": "Yufolo did not return an answer.",
  "资料已变更，请重新生成": "The materials changed. Generate again.",
  "资料已变更，请重试": "The materials changed. Try again.",
  "文档损坏或无法解析": "The document is damaged or could not be parsed.",
  "文档文字过多，请拆分为较小的文件": "The document has too much text. Split it into smaller files.",
  "请使用 UTF-8 编码的文本文件": "Use a UTF-8 text file.",
  "JSON 文件格式不正确": "The JSON file is not valid.",
  "HTML 文件无法解析": "The HTML file could not be parsed.",
  "DOCX 文件无法解析": "The DOCX file could not be parsed.",
  "DOCX 解压内容过大": "The unpacked DOCX is too large.",
  "DOCX 文件损坏": "The DOCX file is damaged.",
  "DOCX 文本无法解析": "The DOCX text could not be parsed.",
  "DOCX 缺少正文": "The DOCX file has no body text.",
  "XLSX 文件无法解析或解压内容过大": "The XLSX file could not be parsed, or its unpacked contents are too large.",
  "工作表无法读取": "A worksheet could not be read.",
  "单元格无法读取": "A cell could not be read.",
  "工作表不完整": "A worksheet is incomplete.",
  "PDF 文件无法解析，可能损坏或已加密":
    "The PDF could not be parsed. It may be damaged or encrypted.",
  "PDF 超过 200 页，请拆分上传": "The PDF is longer than 200 pages. Split it before uploading.",
  "支持 PDF、DOCX、TXT、Markdown、HTML、CSV、JSON、XLSX；旧 XLS 请先另存为 XLSX":
    "PDF, DOCX, TXT, Markdown, HTML, CSV, JSON, and XLSX are supported. Save older XLS files as XLSX first.",
  "未提取到文字；扫描 PDF 请先进行 OCR":
    "No text was extracted. Run OCR on a scanned PDF first.",
  "文档文字过多或编码无效，请拆分或转换后上传":
    "The document has too much text or an invalid encoding. Split or convert it, then upload again.",
  "AI 接口地址无效": "The AI endpoint address is invalid.",
  "AI 服务连接失败或超时，请稍后重试": "The AI service failed to connect or timed out. Try again later.",
  "AI 返回内容过大或不完整": "The AI response was too large or incomplete.",
  "AI 服务返回了无法解析的内容": "The AI service returned content that could not be parsed.",
  "尚未配置 AI 聊天接口、密钥和模型": "The AI chat endpoint, key, and model are not configured.",
  "AI 未返回回答内容": "The AI did not return an answer.",
  "AI 回答过长，请调整提示词后重试": "The AI answer was too long. Adjust the prompt and try again.",
  "向量数量与文本数量不一致": "The number of vectors does not match the number of texts.",
  "向量服务返回了无效索引或维度": "The embedding service returned an invalid index or dimension.",
  "向量维度不一致": "The vector dimensions do not match.",
  "向量服务返回了无效向量": "The embedding service returned an invalid vector.",
  "上传失败": "Upload failed.",
  "无法读取实时数据，请刷新页面": "Live data could not be read. Refresh the page.",
  "转录连接超时，请检查网络和账号状态。":
    "The transcription connection timed out. Check the network and account.",
  "转录响应格式不正确。": "The transcription response could not be read.",
  "转录中断，请检查连接。": "Transcription was interrupted. Check the connection.",
  "转录连接意外断开，请检查网络；已确认字幕已保留。":
    "The transcription connection closed unexpectedly. Check the network. Confirmed captions were kept.",
  "连接恢复超过音频缓存容量，录音已暂停，请检查网络。":
    "Reconnecting overflowed the audio buffer, so recording paused. Check the network.",
  "最后一段字幕保存超时，请检查连接。":
    "Saving the last caption timed out. Check the connection.",
  "录音交接超时，请检查网络；已确认字幕已保留。":
    "The recording handoff timed out. Check the network. Confirmed captions were kept.",
};

const patterns: Array<[RegExp, (match: RegExpMatchArray) => string]> = [
  [/^PDF 第 (\d+) 页无法读取$/, (match) => `Could not read PDF page ${match[1]}.`],
  [
    /^AI 服务返回 HTTP (\d+)，请检查接口、模型、密钥和额度$/,
    (match) =>
      `The AI service returned HTTP ${match[1]}. Check the endpoint, model, key, and quota.`,
  ],
  [/^Yufolo：([\s\S]*)$/, (match) => `Yufolo: ${match[1]}`],
];

export function localizeError(message: string): string {
  if (!message || getLocale() !== "en") return message;
  const exact = enErrors[message];
  if (exact) return exact;
  for (const [pattern, format] of patterns) {
    const match = message.match(pattern);
    if (match) return format(match);
  }
  return message;
}
