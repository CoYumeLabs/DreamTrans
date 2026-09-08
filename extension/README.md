# DreamTrans Moodle Sync

只在你的浏览器里运行的 Moodle 同步扩展。设计文档见 `docs/plans/moodle-sync.md`。

- 课件在浏览器里抓取和抽取（pdf.js、jszip），服务器只收到按页文本。
- 含图页面可以上传一张约 1024px 的渲染图，服务器 OCR 后立即丢弃，不保存图片。
- 服务器永远不接收 MoodleSession、sesskey 或任何 Moodle 令牌。
- 没有后台轮询：只有你打开 Moodle 课程页并点「同步」时才工作。
- 只读；所有论坛（包括名为公告或新闻的论坛）、提交物、成绩不同步；Leganto / eReserve 跳过。

## 构建

```bash
cd extension
npm install
npm run build        # dist/chrome 与 dist/firefox
npm run package      # 另打出 dist/*.zip
```

## 安装

| 浏览器 | 方式 |
|---|---|
| Chrome | `chrome://extensions` → 打开「开发者模式」→「加载已解压的扩展程序」→ 选 `dist/chrome` |
| Edge | `edge://extensions` → 同上，或提交 `dist/dreamtrans-moodle-sync-chrome.zip` 到 Edge Add-ons |
| Firefox | `about:debugging#/runtime/this-firefox` → 「临时载入附加组件」→ 选 `dist/firefox/manifest.json`；长期使用需用 AMO 签名后自托管 `.xpi` |

## 使用

1. 打开 Moodle 课程页（`course/view.php?id=…`），点扩展图标。
2. 第一次：填 DreamTrans 服务器地址、邮箱、密码登录。浏览器会请求访问该服务器的权限，这是后台上传派生文本所需的。
3. 选择要同步到的 DreamTrans 课程（会记住这门 Moodle 课程的对应关系）。
4. 「诊断」跑 PRD 第 6 节的验证清单，生成可复制的 Markdown 特征表，不上传任何内容。
5. 「同步本课程」按 `timemodified` 增量拉取；同一文件（sha256）不会重复上传。同步过程中可以关掉弹窗。

## 代码结构

```
src/content/   注入 Moodle 页的脚本：moodle-cfg（读 M.cfg）→ discovery（AJAX → HTML → 链接）→ fetcher（按 modtype 取）→ extract（浏览器内抽取）→ sync
src/background.ts  唯一与 DreamTrans 对话的代码：登录、刷新令牌、上传派生文本
src/popup/     弹窗界面
src/shared/    类型与消息定义
```

## 还没做的（PRD 里的 M2 / M3）

- Echo360 页面的 content script（读 media 元数据与字幕）与转写 ↔ slides 对齐。
- 新材料推送通知。
- 图像的 VLM 描述（当前只做 OCR）。
