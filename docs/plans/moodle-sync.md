# DreamTrans Moodle Sync — PRD v0.3

只在用户浏览器内运行的 Moodle 同步扩展：拉课件 → 浏览器内抽取 → 上传 DreamTrans → 与录播转写按页对齐。服务器不持有 Moodle 凭据，课件图像不持久保存。

## 1. 对 Asksia 的差异

| | Asksia | 我们 |
|---|---|---|
| 抓取 | DOM 爬取 | AJAX API 优先，HTML 解析兜底 |
| 同步 | 手动全量 | `timemodified` 增量 + 新材料通知 |
| 存储 | 整份 PDF 上云 | 文本 + 图像描述持久化，图片处理完即删 |
| 录播 | 通用转写 | 转写 ↔ 当周 slides 按页对齐 |

## 2. 架构

```
content script ─① Discovery─② Fetch─③ Extract─▶ DreamTrans API ─④ Process/Align
```

**① Discovery**
`POST /lib/ajax/service.php?sesskey=<M.cfg.sesskey>&info=core_courseformat_get_state` → section / cm 列表（4.0+ 课程目录自身用的接口；`core_course_get_contents` 在核心里没有 ajax 标记，Monash 实测不可用）。state 不含文件列表和 `timemodified`，文件在抓取时从模块页取，增量靠 sha256。课程列表用 `core_course_get_enrolled_courses_by_timeline_classification`。AJAX 失败时解析 HTML：3.x `li.section.main`，4.x `[data-for="section"]`，再兜底扫 `[role="main"] a[href]`。

**② Fetch**

| modtype | 取法 |
|---|---|
| resource | `mod/resource/view.php?id=X` 跟 302 → `pluginfile.php`，文件名取 `response.url` / `Content-Disposition` |
| folder | `mod/folder/download_folder.php?id=X` → zip，本地解压 |
| book | `mod/book/tool/print/index.php?id=X` |
| page / label | 正文 HTML |
| assign | 标题、描述、due date，不取提交物 |
| forum | 仅 news forum |
| url / lti | 识别 Echo360 / Panopto / YouTube，记录为 Recording |
| Leganto / eReserve | 跳过 |

并发 3，间隔 ≥ 200ms。无后台轮询，用户打开 Moodle 时按需增量。

**③ Extract（浏览器内）**
`pdf.js` / `jszip` 按页分类：

| 页类型 | 上传 | 服务端保留 |
|---|---|---|
| 纯文本 | 文本 | 文本 |
| 含图 / 图为主 | 文本 + 页面渲染图 ~1024px | VLM 描述 + OCR + bbox；图片 24h 内删除 |

原文件和渲染图留 IndexedDB，按 sha256 引用，看原图本机渲染。云存图 / 全文件上传为 opt-in。

**④ Process / Align**
sha256 内容寻址去重。转写分段与每页文本+图像描述做 embedding 相似度 + 时序 DP，输出 `[{t0, t1, page}]`。Echo360 侧另有 content script 读 media 元数据和自带字幕作对齐锚点；不抓音频流。

## 3. 数据模型

```
Course      { id, shortname, lms_host, last_synced_at }
Section     { course_id, id, name, order }
Module      { section_id, cmid, modtype, name, url, timemodified, due_at? }
Document    { module_id, sha256, filename, mimetype, page_count }
DocumentText{ sha256, pages: [{n, text, figures: [{bbox, caption, ocr}]}] }
Recording   { module_id, provider, external_id, transcript_id? }
Alignment   { transcript_id, sha256, segments: [{t0, t1, page}] }
```

## 4. 硬约束

1. 服务器不接收 MoodleSession / sesskey / token。
2. 无后台轮询。
3. 只读：manifest 无写权限，代码无向 Moodle 的写路径。
4. 讨论区、提交物、成绩不同步；不跨用户共享；图书馆资源不抓。
5. 图像不持久化，opt-in 除外。

## 5. 分发（不走 Chrome Web Store）

| 渠道 | 方式 | 备注 |
|---|---|---|
| Edge Add-ons | 同一份 MV3 代码提交 | Chromium 内核，审核宽松，Chrome 用户也可从 Edge 商店装 |
| Firefox AMO 自分发 | 签名后自托管 `.xpi` | 无需上架，签名即可安装 |
| Chrome 开发者模式 | 提供 zip + 一页安装说明 | 「加载已解压的扩展程序」，Monash 学生受众可接受 |

manifest 用 `optional_host_permissions`，首次在 Moodle 域上手动授权。

## 6. 上线前在 Monash Moodle 验证

- [x] `core_course_get_contents` AJAX 是否开放 —— 否（2026-09-28 PSY2041：servicenotavailable），改用 `core_courseformat_get_state`
- [x] `sesskey` 在 HTML 中的位置 —— `M.cfg.sesskey`；Moodle 4.1（Panopto instance 名 Moodle-41-Prod）
- [x] 四门课 modtype 分布 —— PSY2041：label 45、cms 28、resource 25、url 12、quiz 12、forum 6、lti 5、folder 2、assign 2、page 1、hsuforum 1；cms（mod_cms）按页面抓取；论坛默认不同步，用户手动开启后只取标题、时间、正文
- [ ] Echo360 嵌入方式、域名、字幕接口 —— PSY2041 用的是 Panopto（`monash.au.panopto.com`，Moodle Panopto block + 播客 feed），未见 Echo360；M2 对齐需改以 Panopto 为主
- [ ] Leganto 链接特征
- [ ] 单课全量请求数与耗时
