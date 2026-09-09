import type { Locale } from '../i18n'

export const LEGAL_EFFECTIVE_DATE = '2026-09-09'
export const LEGAL_OPERATOR = 'Coyume Pty Ltd'
export const LEGAL_PRODUCT = 'Yufolo'
export const LEGAL_CONTACT_EMAIL = 'support@coyume.com'

export type LegalKind = 'privacy' | 'terms'

export type LegalBlock =
  | { type: 'p'; text: string }
  | { type: 'ul'; items: readonly string[] }

export interface LegalSection {
  id: string
  heading: string
  blocks: readonly LegalBlock[]
}

export interface LegalDocument {
  kind: LegalKind
  title: string
  summary: string
  sections: readonly LegalSection[]
}

const privacyZh: LegalDocument = {
  kind: 'privacy',
  title: '隐私政策',
  summary:
    '本政策说明 Yufolo（DreamTrans 软件的对外产品名）如何收集、使用、保存和共享你的信息。它按当前软件的实际数据流撰写，适用于由 Coyume Pty Ltd 运营的官方托管服务。若你使用的是他人自行部署的实例，该部署的运营方才是该实例的个人信息处理者，其政策可能与本文不同。',
  sections: [
    {
      id: 'who',
      heading: '1. 我们是谁',
      blocks: [
        {
          type: 'p',
          text: 'Yufolo 由澳大利亚私营公司 Coyume Pty Ltd（下称「我们」）运营。Yufolo 提供实时语音转录、翻译、会话存档、AI 辅助和学习空间。仓库中的软件工程名称为 DreamTrans。',
        },
        {
          type: 'p',
          text: '就本政策提出的请求，请发送至 support@coyume.com；从你的注册邮箱发出可以帮助我们更快核实身份。',
        },
      ],
    },
    {
      id: 'scope',
      heading: '2. 适用范围',
      blocks: [
        {
          type: 'p',
          text: '本政策适用于网站、工作区、管理后台、学习空间，以及官方浏览器扩展（例如 Moodle 同步）在与 Yufolo 服务器通信时处理的信息。',
        },
        {
          type: 'p',
          text: '它不适用于语音识别、机器翻译、大模型或支付等第三方自己的处理活动——那些服务受其自身隐私政策约束。我们会在下文列出主要处理者。',
        },
      ],
    },
    {
      id: 'collect',
      heading: '3. 我们收集哪些信息',
      blocks: [
        {
          type: 'p',
          text: '我们只收集提供服务所需要的信息，主要包括：',
        },
        {
          type: 'ul',
          items: [
            '账户信息：邮箱、显示名、密码哈希、角色、租户归属、邮箱验证状态。我们不保存明文密码。',
            '音频：你授权后，浏览器会采集麦克风和/或系统/标签页音频。实时音频会经我们的服务器转发到语音识别服务商，用于转录和按音频字节计量。云端会话同步不会上传或保存完整录音文件。若你在设置中开启「保存本地录音」，录音分块只写在这台设备的浏览器 IndexedDB 中。',
            '批量转写：你提交的音频文件会经我们的服务器转交给语音识别服务商。传输和处理期间可能在内存中缓冲或写入服务端临时文件，正常请求处理结束后执行清理；我们不把它作为可下载的云端录音长期保存。服务商的数据用途与留存另见第 6 节。',
            '转录与翻译文字：带说话人、时间戳的原文、译文及相关元数据。访客模式下主要保存在本机；登录后会同步到我们的数据库，并在本机保留按账号隔离的缓存。',
            '知识与学习内容：你主动上传的文件、可编辑记忆、摘要、笔记、行动项、技能地图、练习记录，以及从学习管理系统同步的派生文本。',
            '计费信息：用量、预留与结算、钱包与赠送额度、套餐、充值档位、自动充值的阈值与金额设置。银行卡等支付详情由 Stripe 处理，我们只保存 Stripe 的客户标识，不保存完整卡号。你首次在线充值时，Stripe 会按其规则保存该支付方式，供你之后开启的自动充值使用。',
            '活动与注册赠送：使用邀请链接或邀请码时，我们记录活动、来源渠道、标签及与你账户关联的兑换情况，用于核算赠送和评估渠道效果。这是本服务内的来源归因，不等于跨站广告追踪。系统结合浏览器、网络来源及规范化邮箱历史评估重复领取风险；严格模式下，所有新注册赠送都需人工审核，活动预算也可能使发放暂缓。审核或预算暂停针对赠送权益，不因这一状态阻止登录和正常付费使用。可联系 support@coyume.com 请求复核。',
            '技术与安全日志：IP 地址、User-Agent、请求路径、错误与延迟、Cloudflare Ray ID（若经其网络）、注册频率限制所需的有限连接信息。',
            '你主动提供的设置：界面语言、源/目标语言、翻译引擎与提示词、音频输入偏好等。管理员允许时，你填写的第三方 API 密钥只保存在当前标签页的 sessionStorage / 内存中，关闭标签页或退出登录即清除。',
          ],
        },
      ],
    },
    {
      id: 'audio',
      heading: '4. 音频如何处理（请特别注意）',
      blocks: [
        {
          type: 'p',
          text: 'Yufolo 的转录功能需要处理语音。开始录音前，请了解以下数据流，并取得具体场景和适用法律要求的授权。本节说明本身不替代必要的同意或其他法律依据：',
        },
        {
          type: 'ul',
          items: [
            '实时音频会流经我们的服务器，转发至语音识别服务商（当前官方路径为 Speechmatics 的全球实时接口，由其自动选择处理区域），以便生成文字并按转发的音频字节计费。',
            '我们不会把完整录音作为云端会话的一部分保存或同步。产品界面中的「音频保留在你的设备」指的是：Yufolo 云端存的是文字，不是可下载的云端音轨。',
            '本地录音是可选的，由你在设置中开启，仅存在这台浏览器里。清除站点数据、更换设备或卸载浏览器都会导致本地录音丢失，且无法从云端恢复。',
            '开始录音或上传前，你应向参与者说明音频将用于转录、所选功能以及第 6 节披露的供应商模型训练用途，并取得适用法律要求的同意、许可或其他合法依据。我们无法替你取得其他说话人的同意。',
          ],
        },
      ],
    },
    {
      id: 'use',
      heading: '5. 我们如何使用信息',
      blocks: [
        {
          type: 'p',
          text: '我们出于以下目的使用信息：',
        },
        {
          type: 'ul',
          items: [
            '提供转录、翻译、历史、导出、云端同步、AI 助手、知识库和学习空间。',
            '创建和维护账户，发送验证邮件，鉴权与会话隔离。',
            '计量用量、预留与结算费用，处理充值和会员，防止滥用。',
            '保障服务安全与稳定，诊断故障，执行配额和频率限制。',
            '在你主动发起时生成摘要、笔记、行动项、问答和练习材料。',
            '遵守法律义务，处理争议，执行用户条款。',
          ],
        },
        {
          type: 'p',
          text: '我们不会把你的录音、转录或知识库出售给广告商，也不会用它们投放第三方广告。默认情况下，仅仅开始录音不会触发计费的语义索引或后台摘要；这些需要你明确操作或开启相应选项。',
        },
      ],
    },
    {
      id: 'share',
      heading: '6. 我们与谁共享信息',
      blocks: [
        {
          type: 'p',
          text: '我们不会出售你的个人信息。为提供服务，我们会向以下服务商披露必要的数据。服务商的角色取决于具体处理活动：有些活动按我们的指示进行，有些由服务商为履行自身法律义务、安全或本节说明的其他目的独立开展。第三方政策不免除我们就收集、使用和披露个人信息依法承担的责任。',
        },
        {
          type: 'ul',
          items: [
            '语音识别服务商（官方路径为 Speechmatics）：实时或批量音频，以及为识别所需的语言等配置。实时音频经全球接口自动路由，可能在欧洲、美国、澳大利亚等其支持的区域处理，不限定于欧洲。该服务商也可能将这些数据用于改进其自身模型，详见本节末尾。',
            '大模型 / 翻译接口（由我们或部署运营方配置的 OpenAI 兼容服务）：你主动发送的文本、提示词、检索块和生成指令。Speechmatics 内置机器翻译如被选用，则会把相应文本交给该服务。',
            '支付服务商 Stripe：为结账、订阅、退款和税务处理必要的账户与交易信息。Stripe 在部分活动中按我们的指示处理数据，也会为反欺诈、安全和履行自身法律义务等目的作为独立控制者处理数据；相关活动适用 Stripe 的隐私政策及数据处理协议。',
            '邮件发送服务（Resend 或运营方配置的 SMTP）：邮箱地址和验证 / 通知内容。',
            '基础设施提供方：例如托管、数据库、对象存储、内容分发和反向代理，它们可能处理运维所必需的技术数据。',
          ],
        },
        {
          type: 'p',
          text: '供应商安全保障：根据 Speechmatics 公开的安全说明和 Trust Center，其披露的保障包括 ISO/IEC 27001:2022 信息安全管理体系认证、SOC 2 Type II 审计、AES-256 静态数据加密，以及 TLS 1.2 或以上的传输加密。其 Trust Center 还提供 DPA 及相关安全资料。详情见 https://www.speechmatics.com/security 和 https://speechmatics.safebase.us/。这些信息描述供应商的保障，不表示 Yufolo 自身取得了同样的认证；适用范围以供应商证书、报告及合同为准。',
        },
        {
          type: 'p',
          text: '关于模型训练与「训练计划」：我们在 Speechmatics 有两个供应商账户，一个开启了 Model Training 设置，另一个未开启。只有在新手引导或设置中明确选择「加入训练计划」的用户，其实时与批量音频才会经开启训练的账户提交，并因此获得转录费用折扣（翻译等附加项不参与；比例以你作出选择时引导页、设置页及首页定价区显示的为准，其调整适用条款第 13 节）；未作选择、选择不加入以及未登录的调用，一律经未开启训练的账户处理。经开启训练的账户提交的音频与转录可能被该服务商用于训练和改进模型，并不只用于完成你的转录请求。匿名化、去标识化的适用范围，以及训练数据的后续用途和保存安排，以适用于该账户的供应商协议及说明为准。音频在提交和识别时仍可能包含个人信息，不能因为后续可能采取匿名化措施，就将上传过程视为不涉及个人数据。',
        },
        {
          type: 'p',
          text: '我们自己不使用你的音频、转录或笔记训练模型。你可以随时在设置中加入或退出训练计划，更改只影响之后开始的录音和上传：退出后，新的音频不再经开启训练的账户提交，但不会自动删除此前提交的数据或撤销已有训练成果。加入计划前，你须确认自己有权代录音中的所有说话人作出这一选择，并按第 4 节履行告知义务。若某一部署没有配置未开启训练的账户，则不提供训练计划，所有音频经该部署唯一配置的账户处理，其训练设置由部署运营方决定。数据处理相关问题可联系 support@coyume.com。',
        },
        {
          type: 'p',
          text: '若法律要求，或为保护人身、财产与服务安全所必需，我们也可能披露信息。浏览器扩展在同步课程材料时，只向 Yufolo 服务器上传派生文本（以及用于即时 OCR 的临时渲染图，识别后丢弃）；扩展不会把 Moodle 登录 Cookie 或令牌发送给我们。',
        },
      ],
    },
    {
      id: 'overseas',
      heading: '7. 跨境处理',
      blocks: [
        {
          type: 'p',
          text: '我们是澳大利亚公司。为提供实时识别、AI、支付和邮件，你的信息可能被处理于澳大利亚以外，包括欧洲（语音识别）、美国或其他你或运营方选择的模型 / 支付服务所在地。',
        },
        {
          type: 'p',
          text: '使用本服务，即表示你理解这些处理可能发生在你居住地以外，当地的数据保护法律可能不同。主要接收方及处理用途见第 6 节，供应商的处理受适用合同及其隐私政策约束。欧盟与英国用户的相关说明见第 11 节。',
        },
      ],
    },
    {
      id: 'storage',
      heading: '8. 浏览器本地存储与 Cookie',
      blocks: [
        {
          type: 'p',
          text: 'Yufolo 主要使用浏览器存储，而不是广告 Cookie：',
        },
        {
          type: 'ul',
          items: [
            '注册安全 Cookie：服务端签名的随机浏览器标识（30 天），用于识别重复领取注册赠送权益，不跨站追踪。注册时还会读取 UA、平台、语言、时区、屏幕尺寸档位、硬件并发数、触点数及自动化标志，用于与网络来源结合评估重复领取风险；不使用 Canvas、音频或字体探测。',
            'localStorage：登录令牌、界面语言、非密钥设置、引导状态、有限的聊天记录。',
            'sessionStorage：标签页级的第三方 API 密钥（若管理员允许）以及少量进行中的任务状态。',
            'IndexedDB：会话元数据、转录、可选本地录音分块、待同步的云端写入队列。登录后按账号隔离；访客数据与登录数据分开。',
          ],
        },
        {
          type: 'p',
          text: '这些存储是提供登录态、长会话和离线重试所必需的。你可以在浏览器中清除站点数据，但这会删除本机录音、未同步队列和本地历史。我们目前不使用第三方广告追踪或跨站营销 Cookie。',
        },
      ],
    },
    {
      id: 'retention',
      heading: '9. 保存期限',
      blocks: [
        {
          type: 'ul',
          items: [
            '账户：在账户存续期间保存；注销时删除或去标识化提供账户服务所需的资料。依法必须保留的交易记录，以及为处理争议或防止重复领取赠送而仍有必要保留的有限记录，按其具体目的另行处理，不继续用于提供已注销账户的日常服务。',
            '云端会话文字：保存至你删除该会话，或注销账户为止。删除会话会同时删除对应的云端转录副本。我们目前不会按时间自动清理历史会话。',
            '本地录音与缓存：直到你删除会话、清除站点数据，或浏览器回收存储。',
            '计费账本与发票相关记录：为财务、税务和争议处理，在适用法律要求的期限内保留。',
            '验证邮件令牌：短期有效（当前为 24 小时）。',
            '安全与访问日志：保留为诊断和滥用防范所需的合理期限。注册风控保存浏览器标识、浏览器特征组合及网络地址/网段的带密钥摘要，以及粗略浏览器/平台类别、规则命中与关联计数，满 30 天后由每日任务清除这些关联标识；保留审核记录和规范化邮箱的带密钥摘要以防删除账号后重复领取赠送权益。',
            '知识文件与向量索引：通常保存至你删除对应项目、来源或账户；部分文件通过后台删除队列清理，提交删除请求不表示所有物理副本已经即时清除。',
          ],
        },
      ],
    },
    {
      id: 'rights',
      heading: '10. 你的权利',
      blocks: [
        {
          type: 'p',
          text: '在适用法律允许的范围内，你可以：',
        },
        {
          type: 'ul',
          items: [
            '查阅和更正账户资料（工作区内的账户与个人资料）。',
            '导出转录与译文（原文、译文或双语文本）；在本机保存了录音时，也可从该设备下载音频。',
            '删除单个会话；删除后云端文字副本一并删除，本机录音从未上传故无法从云端抹除（请在本机删除或清除站点数据）。',
            '撤回非必要处理：例如退出训练计划、关闭本地录音、关闭自动 AI 入库、停止使用 AI 功能。实时转录本身依赖语音处理，无法在继续使用该功能的同时完全撤回。',
            '请求删除整个账户：当前产品未提供自助注销，请从注册邮箱发信至 support@coyume.com，我们会人工处理。',
          ],
        },
        {
          type: 'p',
          text: '澳大利亚用户可依据《1988 年隐私法》及澳大利亚隐私原则行使权利。若你对我们的处理不满意，可向 Office of the Australian Information Commissioner（OAIC）投诉。其他司法辖区的用户也可按当地法律提出请求；我们将在核实身份后合理期限内答复。',
        },
      ],
    },
    {
      id: 'gdpr',
      heading: '11. 欧盟与英国用户',
      blocks: [
        {
          type: 'p',
          text: '本服务面向全球用户。欧盟 GDPR、英国 GDPR、瑞士及其他地区的数据保护法律是否适用，取决于相应法律的适用范围和具体处理活动，并非只取决于网站能否在当地访问。在相关法律适用时，本节补充说明你的权利。Coyume Pty Ltd 就其决定处理目的和方式的账户、计费及安全等活动承担控制者责任；组织客户及自行部署运营方的角色须按各自活动确定。',
        },
        {
          type: 'p',
          text: '我们处理你的个人数据依据以下法律基础：',
        },
        {
          type: 'ul',
          items: [
            '履行合同（第 6(1)(b) 条）：账户与鉴权、实时与批量转录、翻译、云端文字同步、导出、学习空间、用量计量与结算。不做这些处理就无法向你交付服务。',
            '同意（第 6(1)(a) 条）：在具体处理以同意为法律依据时，你享有适用法律规定的撤回权利，撤回不影响此前处理的合法性。',
            '合法利益（第 6(1)(f) 条）：服务安全、滥用与欺诈防范、故障诊断、配额与频率限制。你可依适用法律对基于合法利益的处理提出反对。',
            '法律义务（第 6(1)(c) 条）：财务、税务与争议处理所需的记录保存。',
          ],
        },
        {
          type: 'p',
          text: '特殊类别数据（第 9 条）：录音可能包含健康、宗教信仰、政治观点等敏感信息。若你计划在可能涉及此类内容的场合录音，你须取得适用法律要求的同意、许可或其他合法依据。',
        },
        {
          type: 'p',
          text: '训练用途与用户选择：向 Speechmatics 开启训练的账户提交音频仅在你明确加入训练计划后发生，以你的选择为依据，且可随时在设置中撤回（不影响撤回前处理的合法性）；默认及撤回后均经未开启训练的账户处理。详见第 6 节。录音及上传涉及其他说话人时，你须按第 4 节履行告知并取得适用法律要求的同意、许可或其他合法依据。',
        },
        {
          type: 'p',
          text: '跨境传输：我们位于澳大利亚，供应商也可能在你的居住地以外处理数据。Speechmatics 在其官方 Trust Center（https://speechmatics.safebase.us/）提供 DPA，公开服务条款第 7 节亦载有数据处理约定。具体传输安排以适用于账户、接收地区和处理用途的供应商合同为准。',
        },
        {
          type: 'p',
          text: '除第 10 节列出的权利外，GDPR 还赋予你以下权利：',
        },
        {
          type: 'ul',
          items: [
            '查阅权、更正权与删除权（「被遗忘权」）。',
            '限制处理权，以及对基于合法利益的处理提出反对的权利。',
            '数据可携带权：以结构化、通用、机器可读的格式取得你提供给我们的数据。工作区的导出功能已可导出转录与译文文本。',
            '撤回同意的权利，且不影响撤回前处理的合法性。',
            '在适用法律规定的情形下，对仅基于自动化处理（含画像）并产生法律效果或类似重大影响的决定，享有相应保障。注册与活动系统会自动评估风险或预算并可能暂缓赠送，管理员可以审核和复核；可联系 support@coyume.com 说明异议及请求人工处理。AI 摘要、练习评分和学习建议仅供参考，不是学校或雇主作出的正式评价。',
          ],
        },
        {
          type: 'p',
          text: '行使上述权利请发信至 support@coyume.com。我们会在收到请求后一个月内答复；请求复杂或数量较多时，我们可延长最多两个月，并会在最初一个月内告知你延期及其原因。你也有权向你惯常居住地、工作地或涉嫌违规发生地的数据保护监管机构投诉；英国用户可向 Information Commissioner’s Office（ICO）投诉。',
        },
      ],
    },
    {
      id: 'children',
      heading: '12. 儿童',
      blocks: [
        {
          type: 'p',
          text: 'Yufolo 面向 18 岁以上的用户。我们不故意收集儿童的个人信息。若你认为我们持有儿童的信息，请联系 support@coyume.com，我们会删除。课堂场景下，由成年账户持有人负责取得学生或监护人的必要同意，并遵守所在学校与当地法律。',
        },
      ],
    },
    {
      id: 'security',
      heading: '13. 安全',
      blocks: [
        {
          type: 'p',
          text: '我们采取与服务性质相符的措施，包括密码哈希、JWT 鉴权、按账号隔离的本地与云端数据、传输加密（HTTPS / WSS）、接口限流，以及将运营方配置的密钥保留在服务端。没有任何系统是绝对安全的。请使用强密码，不要把验证链接转给他人，也不要在不信任的设备上开启本地录音后离开。',
        },
      ],
    },
    {
      id: 'selfhost',
      heading: '14. 自行部署',
      blocks: [
        {
          type: 'p',
          text: 'DreamTrans 可以自行部署。自行部署时，部署运营方决定数据库、密钥、模型供应商、邮件和支付配置，并成为该实例的个人信息处理者。本政策描述的是软件默认数据流和官方 Yufolo 服务；自行部署的运营方应发布自己的隐私政策，并对其用户负责。',
        },
      ],
    },
    {
      id: 'changes',
      heading: '15. 政策变更',
      blocks: [
        {
          type: 'p',
          text: '我们可能更新本政策。更新后的版本会在本页面公布，并修改生效日期。若变更实质影响你的权利，我们会通过网站提示或发给注册邮箱的邮件合理通知。',
        },
      ],
    },
    {
      id: 'contact',
      heading: '16. 如何联系我们',
      blocks: [
        {
          type: 'p',
          text: '隐私请求、投诉与删除请求请发送至 support@coyume.com，并说明你的请求类型（查阅、更正、导出、删除账户等）；从注册邮箱发出可以帮助我们核实身份。官方 Yufolo 服务由澳大利亚公司 Coyume Pty Ltd 运营。若你对我们的答复不满意，可按第 10 节所述向 OAIC 投诉。',
        },
      ],
    },
  ],
}

const termsZh: LegalDocument = {
  kind: 'terms',
  title: '用户条款',
  summary:
    '本条款构成你与 Coyume Pty Ltd 之间关于使用 Yufolo 的协议。创建账户、登录或使用服务，即表示你同意本条款。《隐私政策》另行说明个人信息处理，接受条款不等同于对所有数据用途的同意。若你代表组织使用服务，你保证有权使该组织受本条款约束。',
  sections: [
    {
      id: 'accept',
      heading: '1. 接受条款',
      blocks: [
        {
          type: 'p',
          text: '你必须年满 18 周岁，并具有签订合同的行为能力。若你不同意本条款，请不要注册或使用服务。',
        },
        {
          type: 'p',
          text: '自行部署 DreamTrans 的运营方与其用户之间的合同由该运营方自行制定；本条款适用于 Coyume Pty Ltd 运营的官方 Yufolo 服务，并作为软件默认产品规则的说明。',
        },
      ],
    },
    {
      id: 'service',
      heading: '2. 服务说明',
      blocks: [
        {
          type: 'p',
          text: 'Yufolo 提供实时与批量语音转录、翻译、会话历史、导出、可选的云端文字同步、AI 辅助（问答、摘要、笔记、行动项、知识库）以及学习空间。功能是否可用取决于部署配置、你的套餐、余额和网络状况。',
        },
        {
          type: 'p',
          text: '服务依赖第三方语音识别、模型接口和支付基础设施。我们不保证某一第三方持续可用，也不保证识别或翻译结果完全准确。',
        },
      ],
    },
    {
      id: 'account',
      heading: '3. 账户',
      blocks: [
        {
          type: 'ul',
          items: [
            '你须提供真实、可用的邮箱，并完成验证（在要求验证的部署上，未验证账户不能登录，也不会获得试用额度）。',
            '你负责保管密码和验证链接。通过你的账户进行的活动视为你的行为。',
            '一个邮箱只能注册一个账户。我们可能拒绝一次性邮箱或策略不允许的域名。',
            '若我们有合理依据认为账户被滥用、存在欠费或违反本条款，我们可暂停账户，并会在合理可行时告知原因，给你合理期限说明情况或纠正。仅在滥用严重或持续、涉及违法、危及服务安全或其他用户，或欠费经通知后仍未清偿时，我们才会立即暂停或终止账户。终止时，未使用的充值余额按第 7 节处理并退还（可扣除你应付的款项），赠送额度失效。',
          ],
        },
      ],
    },
    {
      id: 'recording',
      heading: '4. 录音、同意与合法使用',
      blocks: [
        {
          type: 'p',
          text: '你对录音内容和你提交的全部材料承担法律责任。你保证并承诺：',
        },
        {
          type: 'ul',
          items: [
            '你在录音或上传前，向参与者说明录制、转录、翻译、存储以及隐私政策第 6 节披露的供应商模型训练用途，并取得适用法律要求的同意、许可或其他合法依据。',
            '你不会使用服务侵犯他人隐私、窃听、未经授权监控，或违反学校、雇主、会场或所在地的录音规则。',
            '你不会上传或生成违法、侵权、骚扰、或试图破解服务的内容。',
            '你不会绕过计费、配额、鉴权或技术限制，不会共享账户以规避套餐限制。',
            '课堂、会议或公共场合使用时，由你负责告知参与者并遵守当地法律；Yufolo 不是你的合规官。',
          ],
        },
      ],
    },
    {
      id: 'content',
      heading: '5. 你的内容',
      blocks: [
        {
          type: 'p',
          text: '你保留对你的音频、转录、译文、上传文件、笔记和学习材料（「用户内容」）的权利。为向你提供服务，你授予我们一项非独占、全球范围、可向处理者再许可的许可：仅为运营、维护、安全保护和改进向你提供的功能而处理用户内容。',
        },
        {
          type: 'p',
          text: '我们不主张对你的课堂录音或笔记的版权，也不使用用户内容训练我们自己的模型。只有当你明确加入训练计划时，你的音频与转录才会经 Speechmatics 开启训练的账户提交并可能用于改进其模型；你可随时在设置中退出，更改只影响之后的录音。具体说明见隐私政策第 6 节。上述内容许可不替代个人数据处理所需的法律依据。你可以导出文字；删除会话会删除对应的 Yufolo 云端文字副本，但不会自动删除供应商已有数据或撤销已有训练成果。可选本地录音副本保存在你的设备上。',
        },
      ],
    },
    {
      id: 'ai',
      heading: '6. AI 功能',
      blocks: [
        {
          type: 'p',
          text: '翻译、问答、摘要、笔记、行动项、技能地图和练习题由自动化模型生成，可能不准确、不完整或过时。它们不是法律、医疗、学术或专业建议，不能替代你自己的判断。你应对采用任何 AI 输出负责。',
        },
        {
          type: 'p',
          text: '为控制费用，语义索引和多数生成任务需要你明确发起或确认。预览接口在默认情况下可以只做不计费的词法预览；只有你要求执行语义检索时才会产生相应费用。',
        },
      ],
    },
    {
      id: 'fees',
      heading: '7. 费用、钱包与订阅',
      blocks: [
        {
          type: 'ul',
          items: [
            '计费账本以美元记账。转录按音频时长（由转发的音频字节折算）、AI 按 token 或约定规则计费。每次调用可先按估算上限预留，完成后按真实用量结算并退回差额。在线支付按结账页显示的币种和汇率收取（官方服务当前以澳元收款），余额与消费仍以美元记录；结账页显示的汇率仅适用于该笔支付。',
            '赠送额度（含注册试用）可能有有效期；钱包充值余额在账户存续期间通常不过期，但不构成可兑现现金。',
            '会员按购买时展示的方案价格、用量折扣及已明确列明的适用限额提供服务，不包含无限转录时长，也不表示普通用户已有的功能变成会员专属。订阅按页面展示的周期收费，可通过 Stripe 客户门户或产品内说明取消；取消后已付权益的终止时间以结账说明和订阅状态为准。',
            '退款申请由人工审核，请从注册邮箱联系 support@coyume.com，并提供订单或交易信息及申请原因。正常交付且已消耗的用量不予退款。未使用的充值余额按以下规则审核退还：以该笔充值实际支付的金额为上限，扣除自该笔充值起的全部消耗用量（不论消耗先扣的是赠送额度还是钱包余额），按原支付币种与金额原路退回，不因汇率变动增减；随该笔充值发放的加赠额度在退款时一并作废，不折算现金。订阅费用按具体订单、使用情况及适用法律审核。赠送额度不可兑换现金。错误或重复扣费、未交付、服务缺陷及其他依法应退款的情形不受前述不退款限制，人工审核不排除你的法定权利。取消订阅本身不等于退款申请。第 3 节所述由我们终止账户，以及第 13 节所述你因不接受条款变更而退出时，未使用的充值余额按该两节约定退还。',
            '训练计划折扣：在提供训练计划的部署上，加入计划的用户在转录费用（不含翻译等附加项）上享受折扣，与会员折扣叠加计算（先按会员折扣、再按训练计划折扣），自加入后开始的录音起生效；退出后按标准价格计费。折扣比例以你作出选择时引导页、设置页及首页定价区显示的为准，其调整属于价格调整，适用第 13 节。',
            '促销、邀请或活动赠送的额度不适用训练计划折扣，按标准价计费，并经未开启训练的账户处理。充值余额（含充值赠送部分）按你的训练计划选择计费。赠送额度先于充值余额扣减；一场会话中赠送额度用完时，该场会话保持原有账户与价格，结束后按会话开始时的选择及优惠比例将付费部分多收的差额退回钱包并记入账单。训练计划是可选项目，我们可能暂停或重新开放；暂停后所有已加入用户自动退出，重新开放时需重新选择加入；变更只影响之后的会话。机构账户一律经未开启训练的账户处理。',
            '价格、加价、套餐和充值档位可由运营方调整；对已完成的结算，以当时有效的价格为准。',
            '若余额不足，相关功能可能被拒绝（例如返回支付所需错误），直到你充值或获得额度。',
            '自动充值：会员可在账户面板开启自动充值，并自行设定触发阈值和每次充值金额。开启后，当某次用量预留因余额低于阈值而失败时，我们会用你在 Stripe 保存的支付方式按你设定的金额扣款并计入钱包，每次扣款都有流水。该功能默认关闭，须由你主动开启，并可随时在账户面板关闭；关闭后不再自动扣款，已充入的余额按本节规则处理。若扣款失败，相关功能会在你手动充值前被拒绝。',
          ],
        },
        {
          type: 'p',
          text: '本条款不排除适用法律规定的不可排除的消费者权利。',
        },
      ],
    },
    {
      id: 'ip',
      heading: '8. 我们的知识产权',
      blocks: [
        {
          type: 'p',
          text: 'Yufolo / DreamTrans 的软件、界面、商标、文案和文档归 Coyume Pty Ltd 或其许可方所有。软件按 PolyForm Noncommercial License 1.0.0 及仓库中的许可文件提供；未经许可的商业使用可能构成侵权。本条款授予你一份有限的、不可转让的、可撤销的使用官方服务的许可，不转让任何知识产权。',
        },
      ],
    },
    {
      id: 'thirdparty',
      heading: '9. 第三方服务',
      blocks: [
        {
          type: 'p',
          text: '语音识别、模型接口、支付和邮件由第三方提供。你与这些第三方之间还可能受其条款约束。因第三方中断、拒绝服务、价格变化或政策变化导致的功能不可用，我们在法律允许范围内不承担责任，但会合理努力告知受影响的功能。',
        },
      ],
    },
    {
      id: 'disclaimer',
      heading: '10. 免责声明',
      blocks: [
        {
          type: 'p',
          text: '在适用法律允许的范围内，服务按「现状」和「可用」提供。我们不保证服务不中断、无错误，也不保证转录、翻译或 AI 输出适合某一特定用途。实时识别依赖网络和第三方；断网时本机可能仍保存已确认的文字，但不等于可以离线继续识别。',
        },
      ],
    },
    {
      id: 'liability',
      heading: '11. 责任限制',
      blocks: [
        {
          type: 'p',
          text: '本节不限制适用法律规定不得排除或限制的责任。在法律允许排除的范围内，我们不对间接或后果性损失承担责任。对于依法可以限制的责任，我们就同一事件或相互关联事件承担的合计责任，以事件发生当月你使用相关服务实际消耗的现金充值余额及归属于该月的相关订阅费用为限，不因充值或订阅付款发生在此前月份而排除计算；赠送额度不计入。',
        },
        {
          type: 'p',
          text: '若第三方因你违反本条款、非法录音或侵犯其权利而向我们提出索赔，你须在该索赔由你的行为直接导致的范围内，赔偿我们因此产生的合理损失、费用和合理的法律费用。赔偿责任按你的过错比例确定，不包括由我们自身违约、过失或违法行为造成的部分。我们会及时通知你相关索赔，并合理配合抗辩。本段在适用法律允许的范围内适用，不减损你作为消费者的法定权利。',
        },
      ],
    },
    {
      id: 'term',
      heading: '12. 期限与终止',
      blocks: [
        {
          type: 'p',
          text: '你可随时停止使用服务并删除会话。账户删除请发信至 support@coyume.com。我们可因维护、安全、违法或严重违约立即暂停服务。条款中依其性质应继续有效的部分（包括费用、知识产权、免责和责任限制）在终止后仍然有效。',
        },
      ],
    },
    {
      id: 'changes',
      heading: '13. 条款变更',
      blocks: [
        {
          type: 'p',
          text: '我们可能因法律要求、安全需要、功能变化或经营原因更新本条款，并在本页面公布新版本及其生效日期。若变更实质影响你的权利或费用，我们会至少提前 30 天通过网站提示或注册邮箱通知你；为遵守法律或处理紧急安全问题而作的变更，通知期可能缩短。若你不接受变更，可在生效日期前停止使用并发信至 support@coyume.com 关闭账户：我们会退还未使用的充值余额，正在进行的订阅按剩余周期比例退款，赠送额度不予兑现。在生效日期之后继续使用服务，视为接受更新后的条款；这不影响你上述的退出权和不可排除的法定权利。价格调整另有以下规则：仅适用于新充值和新订阅的价格调整自公布起生效；影响已购余额消耗速度的费率上调，若我们在切换时对已有余额按同一比例补足、使其可用量不减少，则对已有余额不构成实质不利变更，可自公布起生效，我们会同时告知你；其他影响已购余额或现有订阅续费的调整，适用上述 30 天通知。',
        },
      ],
    },
    {
      id: 'law',
      heading: '14. 适用法律',
      blocks: [
        {
          type: 'p',
          text: '本条款受澳大利亚法律管辖。在消费者强制管辖规则允许的范围内，双方接受澳大利亚法院的管辖。若某一条款被认定为不可执行，其余部分仍然有效。',
        },
      ],
    },
    {
      id: 'contact',
      heading: '15. 联系我们',
      blocks: [
        {
          type: 'p',
          text: '关于本条款的问题，请发送至 support@coyume.com。官方 Yufolo 服务由澳大利亚公司 Coyume Pty Ltd 运营。',
        },
      ],
    },
  ],
}

const privacyEn: LegalDocument = {
  kind: 'privacy',
  title: 'Privacy Policy',
  summary:
    'This policy explains how Yufolo (the customer-facing name of the DreamTrans software) collects, uses, stores and shares your information. It follows the product’s actual data flows and applies to the official hosted service operated by Coyume Pty Ltd. If you use a self-hosted instance, that operator is the controller for that instance and may publish different terms.',
  sections: [
    {
      id: 'who',
      heading: '1. Who we are',
      blocks: [
        {
          type: 'p',
          text: 'Yufolo is operated by Coyume Pty Ltd, an Australian proprietary company (“we”). Yufolo provides live speech transcription, translation, session history, AI assistance and a study workspace. The engineering name of the software is DreamTrans.',
        },
        {
          type: 'p',
          text: 'Privacy requests can be sent to support@coyume.com. Writing from the address on your account helps us verify you faster.',
        },
      ],
    },
    {
      id: 'scope',
      heading: '2. Scope',
      blocks: [
        {
          type: 'p',
          text: 'This policy covers the website, workspace, admin dashboard, study space, and official browser extensions (such as Moodle sync) when they talk to a Yufolo server.',
        },
        {
          type: 'p',
          text: 'It does not cover the independent processing of speech-recognition, model, or payment providers — those services have their own privacy policies. The main processors are listed below.',
        },
      ],
    },
    {
      id: 'collect',
      heading: '3. Information we collect',
      blocks: [
        {
          type: 'p',
          text: 'We collect what we need to run the service, including:',
        },
        {
          type: 'ul',
          items: [
            'Account data: email, display name, password hash, role, tenant, verification status. We do not store plaintext passwords.',
            'Audio: after you grant permission, the browser captures microphone and/or system/tab audio. Live audio is forwarded through our servers to the speech-recognition provider for transcription and byte-based metering. Cloud session sync does not upload or keep a full recording. If you enable local audio saving, chunks are written only to IndexedDB on that device.',
            'Batch files: audio you submit passes through our servers to the speech-recognition provider. It may be buffered in memory or written to temporary server files during transfer and processing; cleanup runs when normal request handling ends. We do not retain it as a downloadable cloud recording. Provider uses and retention are described in section 6.',
            'Transcripts and translations: speaker-attributed text, timestamps and related metadata. Guest mode keeps this mainly on-device; signed-in sessions sync text to our database and keep an account-scoped browser cache.',
            'Knowledge and study content: files you upload, editable memories, summaries, notes, action items, skill maps, practice records, and derived text synced from a learning system.',
            'Billing data: usage, reservations and settlements, wallet and grant balances, plans, top-ups, and your automatic top-up threshold and amount. Card details are handled by Stripe; we keep only the Stripe customer identifier, never full card numbers. When you first pay online, Stripe saves that payment method under its rules so that automatic top-up, if you later turn it on, can use it.',
            'Promotions and signup rewards: when you use an invitation link or code, we record the promotion, source channel, tags and account-linked redemption to administer rewards and measure channel performance. This is attribution within our service, not cross-site advertising tracking. Browser, network and normalized email history help assess repeated claims. In strict mode all new signup rewards require manual review; campaign budgets can also delay fulfilment. These review or budget holds apply to promotional benefits and do not themselves block sign-in or ordinary paid use. Contact support@coyume.com to request review.',
            'Technical and security logs: IP address, user agent, request path, errors and latency, Cloudflare Ray ID when present, and limited connection data used for sign-up rate limits.',
            'Settings you choose: interface language, source/target languages, translation engine and prompts, audio-input preferences. When an administrator allows a bring-your-own API key, that secret is kept only in this tab’s sessionStorage or memory and is cleared on logout or when the tab closes.',
          ],
        },
      ],
    },
    {
      id: 'audio',
      heading: '4. How audio is handled',
      blocks: [
        {
          type: 'p',
          text: 'Yufolo transcription requires speech processing. Before recording, understand the following data flows and obtain the authorisations required for your circumstances and applicable law. This explanation does not itself replace necessary consent or another lawful basis:',
        },
        {
          type: 'ul',
          items: [
            'Live audio is streamed through our servers to the speech-recognition provider (the official path uses Speechmatics’ global real-time endpoint, which automatically selects the processing region) so we can produce text and meter forwarded audio bytes.',
            'We do not store or sync the full recording as part of a cloud session. When the product says audio stays on your device, it means Yufolo’s cloud copy is text, not a downloadable cloud soundtrack.',
            'Local recordings are optional, enabled in settings, and exist only in that browser. Clearing site data, switching devices or removing the browser deletes them; they cannot be restored from the cloud.',
            'Before recording or uploading, explain to participants that audio will be processed for transcription, selected features and the provider model-training use disclosed in section 6, and obtain the consent, permissions or other lawful basis required by applicable law. We cannot obtain consent from other speakers for you.',
          ],
        },
      ],
    },
    {
      id: 'use',
      heading: '5. How we use information',
      blocks: [
        {
          type: 'p',
          text: 'We use information to:',
        },
        {
          type: 'ul',
          items: [
            'Provide transcription, translation, history, export, cloud text sync, the AI assistant, knowledge projects and the study space.',
            'Create and maintain accounts, send verification email, authenticate and isolate sessions.',
            'Meter usage, reserve and settle charges, process top-ups and memberships, and prevent abuse.',
            'Keep the service secure and reliable, diagnose faults, and enforce quotas and rate limits.',
            'Generate summaries, notes, action items, answers and practice material when you ask.',
            'Meet legal duties, handle disputes and enforce the Terms of Use.',
          ],
        },
        {
          type: 'p',
          text: 'We do not sell your recordings, transcripts or knowledge base to advertisers, and we do not use them for third-party ads. Starting a recording does not, by itself, start a paid semantic index or background summary; those need an explicit action or a setting you turn on.',
        },
      ],
    },
    {
      id: 'share',
      heading: '6. Who we share information with',
      blocks: [
        {
          type: 'p',
          text: 'We do not sell your personal information. We disclose necessary data to the providers below to deliver the service. Their roles depend on the activity: some processing follows our instructions, while providers independently carry out other activities for their own legal obligations, security or other purposes described in this section. Their policies do not remove our legal responsibilities for collecting, using or disclosing personal information.',
        },
        {
          type: 'ul',
          items: [
            'Speech-recognition provider (official path: Speechmatics): live or batch audio and recognition settings. Live audio is automatically routed through the global endpoint and may be processed in Europe, the United States, Australia or other supported regions; processing is not restricted to Europe. This provider may also use that data to improve its own models — see the end of this section.',
            'Model / translation APIs (OpenAI-compatible services configured by us or the deployment operator): text, prompts, retrieved chunks and generation instructions you send. If you choose Speechmatics machine translation, that text is sent there instead.',
            'Stripe: account and transaction data needed for checkout, subscriptions, refunds and tax. Stripe processes some data on our instructions and also acts as an independent controller for activities such as fraud prevention, security and its own legal obligations. Those activities are described in its Privacy Policy and Data Processing Agreement.',
            'Email delivery (Resend or operator-configured SMTP): your address and verification or notice content.',
            'Infrastructure providers such as hosting, databases, object storage, CDNs and reverse proxies, which may process technical data required to operate the service.',
          ],
        },
        {
          type: 'p',
          text: 'Provider safeguards: Speechmatics publicly reports ISO/IEC 27001:2022 information-security management certification, a SOC 2 Type II audit, AES-256 encryption at rest and TLS 1.2 or higher in transit. Its Trust Center also offers a DPA and security documentation. See https://www.speechmatics.com/security and https://speechmatics.safebase.us/. These describe provider safeguards, not equivalent certification of Yufolo itself; scope depends on the provider certificates, reports and contracts.',
        },
        {
          type: 'p',
          text: 'On model training and the training programme: we hold two Speechmatics provider accounts, one with Model Training enabled and one without. Only users who explicitly choose to join the training programme, in onboarding or in Settings, have their live and batch audio submitted through the training-enabled account, and they receive a transcription discount in return (not applied to add-ons such as translation; the rate is the one shown in onboarding, Settings and the pricing section when you choose, and changes to it follow section 13 of the Terms). Users who have not answered, who declined, and calls made without signing in all go through the account without training. Audio and transcripts submitted through the training-enabled account may be used by the provider to train and improve models, beyond completing your transcription request. The scope of anonymisation or de-identification, subsequent use and retention depends on the provider agreement and documentation applicable to that account. Audio may still contain personal information when submitted and transcribed; possible subsequent anonymisation does not make the upload process free of personal data.',
        },
        {
          type: 'p',
          text: 'We do not use your audio, transcripts or notes to train our own models. You can join or leave the training programme in Settings at any time; the change applies to recordings and uploads that start afterwards. Leaving stops new audio going through the training-enabled account but does not automatically delete previously submitted data or undo training already performed. Before joining, you must be entitled to make that choice for everyone who speaks in your recordings and give the notice described in section 4. A deployment without an account that has training switched off does not offer the programme; all audio then goes through the single configured account, whose training setting is the operator’s decision. For questions about data processing, contact support@coyume.com.',
        },
        {
          type: 'p',
          text: 'We may also disclose information if the law requires it, or if it is necessary to protect people, property or the service. The browser extension uploads derived page text to Yufolo (and a temporary render used for on-the-spot OCR, which is discarded afterwards). It does not send Moodle session cookies or tokens to us.',
        },
      ],
    },
    {
      id: 'overseas',
      heading: '7. Overseas processing',
      blocks: [
        {
          type: 'p',
          text: 'We are an Australian company. To provide recognition, AI, payments and email, your information may be processed outside Australia, including Europe (speech recognition), the United States, or another region chosen for the model or payment provider you or the operator configure.',
        },
        {
          type: 'p',
          text: 'By using the service you understand that processing may occur outside your country, where privacy laws can differ. Section 6 describes principal recipients and purposes; provider processing is subject to applicable contracts and their privacy policies. Section 11 has further information for users in the EU and UK.',
        },
      ],
    },
    {
      id: 'storage',
      heading: '8. Local browser storage and cookies',
      blocks: [
        {
          type: 'p',
          text: 'Yufolo relies on browser storage rather than advertising cookies:',
        },
        {
          type: 'ul',
          items: [
            'Signup security cookie: a server-signed random browser identifier (30 days) used to detect repeated signup rewards without cross-site tracking. At signup we also read the user agent, platform, language, time zone, coarse screen dimensions, hardware concurrency, touch-point count and automation flag to assess repeated rewards together with network signals. We do not probe canvas, audio or fonts.',
            'localStorage: sign-in tokens, interface language, non-secret settings, onboarding state and a limited chat history.',
            'sessionStorage: a tab-scoped third-party API key when allowed, and a little in-flight task state.',
            'IndexedDB: session metadata, transcripts, optional local audio chunks, and the pending cloud-write outbox. Signed-in data is account-scoped; guest data stays separate.',
          ],
        },
        {
          type: 'p',
          text: 'This storage is required for sign-in, long sessions and offline retries. You can clear site data in the browser; that also deletes local recordings, unsynced writes and local history. We do not currently use third-party advertising or cross-site marketing cookies.',
        },
      ],
    },
    {
      id: 'retention',
      heading: '9. Retention',
      blocks: [
        {
          type: 'ul',
          items: [
            'Accounts: retained while the account exists; closure removes or de-identifies information needed to provide the account service. Transaction records required by law, and limited records still necessary for disputes or preventing repeated reward claims, are handled separately for those purposes rather than used to continue ordinary service for the closed account.',
            'Cloud session text: kept until you delete the session or close the account. Deleting a session deletes the matching cloud transcript. We do not currently purge old sessions on a timer.',
            'Local recordings and caches: until you delete the session, clear site data, or the browser reclaims storage.',
            'Billing ledgers and invoice-related records: kept for the period required for finance, tax and disputes.',
            'Email verification tokens: short-lived (currently 24 hours).',
            'Security and access logs: kept for a reasonable period for diagnosis and abuse prevention. Signup risk stores keyed hashes of browser identifiers, browser characteristic combinations and network addresses/prefixes, along with coarse browser/platform categories, rule matches and correlation counts, clearing those correlation identifiers in a daily job after 30 days. Review records and keyed hashes of normalized emails remain to prevent repeated rewards after account deletion.',
            'Knowledge files and vector indexes: generally retained until you delete the project, source or account. Some files are removed through background deletion queues, so submitting a deletion request does not mean every physical copy is immediately erased.',
          ],
        },
      ],
    },
    {
      id: 'rights',
      heading: '10. Your rights',
      blocks: [
        {
          type: 'p',
          text: 'Where applicable law allows, you may:',
        },
        {
          type: 'ul',
          items: [
            'Access and correct account details in the workspace.',
            'Export transcripts and translations (original, translated or bilingual text) and, if a local recording exists on that device, download the audio.',
            'Delete individual sessions. That removes the cloud text copy. Local audio was never uploaded, so delete it on the device or clear site data.',
            'Withdraw optional processing: leave the training programme, turn off local audio saving, turn off automatic AI ingest, or stop using AI features. Live transcription itself requires speech processing and cannot be fully withdrawn while you keep using that feature.',
            'Ask us to delete the whole account. There is no self-serve close-account button today; write to support@coyume.com from your registered email and we will handle it.',
          ],
        },
        {
          type: 'p',
          text: 'People in Australia may exercise rights under the Privacy Act 1988 (Cth) and the Australian Privacy Principles. If you are not satisfied, you may complain to the Office of the Australian Information Commissioner (OAIC). Users elsewhere may make requests under local law. We will respond within a reasonable time after verifying your identity.',
        },
      ],
    },
    {
      id: 'gdpr',
      heading: '11. Users in the EU and UK',
      blocks: [
        {
          type: 'p',
          text: 'The service is offered globally. Whether the EU GDPR, UK GDPR, Swiss or other data-protection laws apply depends on their territorial scope and the processing activity, not simply whether the website is accessible locally. This section supplements your rights where those laws apply. Coyume Pty Ltd is responsible as controller for activities whose purposes and means it determines, such as accounts, billing and security. The roles of organisational customers and self-hosted operators depend on their activities.',
        },
        {
          type: 'p',
          text: 'We rely on these legal bases:',
        },
        {
          type: 'ul',
          items: [
            'Performance of a contract (Art. 6(1)(b)): accounts and authentication, live and batch transcription, translation, cloud text sync, export, the study space, and usage metering and settlement. Without this processing we cannot deliver the service you signed up for.',
            'Consent (Art. 6(1)(a)): where a particular activity relies on consent, you have the withdrawal rights provided by applicable law, without affecting the lawfulness of prior processing.',
            'Legitimate interests (Art. 6(1)(f)): service security, abuse and fraud prevention, fault diagnosis, quotas and rate limits. You may object to processing based on legitimate interests under applicable law.',
            'Legal obligation (Art. 6(1)(c)): records we must keep for finance, tax and disputes.',
          ],
        },
        {
          type: 'p',
          text: 'Special category data (Art. 9): recordings may contain sensitive information such as health, religious beliefs or political opinions. If you plan to record where such content may arise, you must obtain the consent, permissions or other lawful basis required by applicable law.',
        },
        {
          type: 'p',
          text: 'Training and user choice: audio reaches the training-enabled Speechmatics account only after you explicitly join the training programme, on the basis of that choice, and you can withdraw it in Settings at any time without affecting the lawfulness of earlier processing; by default, and after you leave, audio goes through the account without training. See section 6. Where recordings or uploads include other speakers, you must provide the notice and obtain the consent, permissions or other lawful basis required by applicable law as described in section 4.',
        },
        {
          type: 'p',
          text: 'International transfers: we are in Australia, and providers may process data outside your country. Speechmatics offers a DPA through its official Trust Center (https://speechmatics.safebase.us/), and section 7 of its public terms also contains data-processing provisions. Specific transfer arrangements depend on the provider contract applicable to the account, destination and processing purpose.',
        },
        {
          type: 'p',
          text: 'Alongside the rights in section 10, the GDPR gives you:',
        },
        {
          type: 'ul',
          items: [
            'Rights of access, rectification and erasure (“the right to be forgotten”).',
            'The right to restrict processing, and to object to processing based on legitimate interests.',
            'Data portability: to receive the data you provided in a structured, commonly used, machine-readable format. The workspace already exports transcripts and translations as text.',
            'The right to withdraw consent, without affecting the lawfulness of processing before withdrawal.',
            'Where applicable law provides, safeguards concerning decisions based solely on automated processing, including profiling, that produce legal or similarly significant effects. Our signup and promotion systems automatically assess risk or budgets and may hold rewards, with administrator review available. Contact support@coyume.com to raise an objection and request human review. AI summaries, practice scores and learning suggestions are not formal assessments by a school or employer.',
          ],
        },
        {
          type: 'p',
          text: 'To exercise these rights write to support@coyume.com. We answer within one month of receiving a request; where a request is complex or there are several, we may extend by up to two further months and will tell you why within the first month. You may also complain to the supervisory authority where you live, where you work, or where you think a breach occurred; in the UK that is the Information Commissioner’s Office (ICO).',
        },
      ],
    },
    {
      id: 'children',
      heading: '12. Children',
      blocks: [
        {
          type: 'p',
          text: 'Yufolo is for users 18 and over. We do not knowingly collect children’s personal information. If you believe we hold a child’s information, contact support@coyume.com and we will delete it. In a classroom, the adult account holder is responsible for student or guardian consent and for school and local rules.',
        },
      ],
    },
    {
      id: 'security',
      heading: '13. Security',
      blocks: [
        {
          type: 'p',
          text: 'We use measures appropriate to the service, including password hashing, JWT authentication, account-scoped local and cloud data, HTTPS/WSS in transit, rate limits, and keeping operator-configured provider keys on the server. No system is perfectly secure. Use a strong password, do not forward verification links, and do not leave local audio saving switched on when you walk away from a device you do not control.',
        },
      ],
    },
    {
      id: 'selfhost',
      heading: '14. Self-hosted deployments',
      blocks: [
        {
          type: 'p',
          text: 'DreamTrans can be self-hosted. The operator then chooses the database, secrets, model vendor, mail and payments, and is the controller for that instance. This policy describes default software data flows and the official Yufolo service. Self-hosted operators should publish their own privacy policy and are responsible to their users.',
        },
      ],
    },
    {
      id: 'changes',
      heading: '15. Changes',
      blocks: [
        {
          type: 'p',
          text: 'We may update this policy. The new version will be posted on this page with a new effective date. If a change materially affects your rights, we will give reasonable notice on the site or by email. ',
        },
      ],
    },
    {
      id: 'contact',
      heading: '16. Contact',
      blocks: [
        {
          type: 'p',
          text: 'Send privacy requests, complaints and deletion requests to support@coyume.com, and say what you want (access, correction, export, account deletion); writing from your registered email helps us verify you. The official Yufolo service is operated by Coyume Pty Ltd in Australia. If our answer does not satisfy you, you may complain to the OAIC as described in section 10.',
        },
      ],
    },
  ],
}

const termsEn: LegalDocument = {
  kind: 'terms',
  title: 'Terms of Use',
  summary:
    'These terms are the agreement between you and Coyume Pty Ltd for using Yufolo. Creating an account, signing in or using the service means you accept these terms. The Privacy Policy separately explains personal-data processing; accepting the terms does not constitute consent to every data use. If you use the service for an organisation, you confirm you can bind that organisation.',
  sections: [
    {
      id: 'accept',
      heading: '1. Acceptance',
      blocks: [
        {
          type: 'p',
          text: 'You must be 18 or older and able to enter a contract. If you do not agree, do not register or use the service.',
        },
        {
          type: 'p',
          text: 'A self-hosted DreamTrans operator sets the contract with its own users. These terms apply to the official Yufolo service operated by Coyume Pty Ltd, and describe the product’s default rules.',
        },
      ],
    },
    {
      id: 'service',
      heading: '2. The service',
      blocks: [
        {
          type: 'p',
          text: 'Yufolo provides live and batch speech transcription, translation, history, export, optional cloud text sync, AI assistance (chat, summaries, notes, action items, knowledge) and a study space. What you can use depends on the deployment, your plan, your balance and the network.',
        },
        {
          type: 'p',
          text: 'The service depends on third-party speech recognition, model APIs and payments. We do not guarantee that a given provider stays available, or that recognition or translation is perfectly accurate.',
        },
      ],
    },
    {
      id: 'account',
      heading: '3. Accounts',
      blocks: [
        {
          type: 'ul',
          items: [
            'You must give a real email address you can access and complete verification where the deployment requires it. On those deployments an unverified account cannot sign in and does not receive trial credit.',
            'You are responsible for your password and verification links. Activity through your account is treated as yours.',
            'One email may register one account. We may refuse disposable addresses or domains blocked by policy.',
            'If we have reasonable grounds to believe an account is abused, unpaid or in breach of these terms, we may suspend it. Where practicable we will tell you why and give you a reasonable period to respond or fix the problem. We close or suspend an account immediately only where the abuse is serious or continuing, involves unlawful activity, endangers the service or other users, or where an unpaid amount remains outstanding after notice. On closure, unused purchased credit is handled and refunded under section 7 (less amounts you owe); promotional credit lapses.',
          ],
        },
      ],
    },
    {
      id: 'recording',
      heading: '4. Recording, consent and acceptable use',
      blocks: [
        {
          type: 'p',
          text: 'You are legally responsible for what you record and upload. You warrant that:',
        },
        {
          type: 'ul',
          items: [
            'Before recording or uploading, you explain recording, transcription, translation, storage and the provider model-training use disclosed in section 6 of the Privacy Policy to participants, and obtain the consent, permissions or other lawful basis required by applicable law.',
            'You will not use the service to invade privacy, intercept communications without authority, or break school, employer, venue or local recording rules.',
            'You will not upload or generate unlawful, infringing, harassing or attack content, or attempt to break the service.',
            'You will not bypass billing, quotas, authentication or technical limits, or share an account to evade plan limits.',
            'In a classroom, meeting or public setting you must inform participants and follow local law. Yufolo is not your compliance officer.',
          ],
        },
      ],
    },
    {
      id: 'content',
      heading: '5. Your content',
      blocks: [
        {
          type: 'p',
          text: 'You keep your rights in your audio, transcripts, translations, uploads, notes and study material (“User Content”). To provide the service you grant us a non-exclusive, worldwide licence, sublicensable to our processors, to process User Content only to operate, maintain, secure and improve the features we provide to you.',
        },
        {
          type: 'p',
          text: 'We do not claim copyright in your classroom recordings or notes and do not train our own models on User Content. Only when you explicitly join the training programme are your audio and transcripts submitted through the training-enabled Speechmatics account, where they may improve its models; you can leave in Settings at any time and the change applies to later recordings. See section 6 of the Privacy Policy. The content licence above does not replace a required lawful basis for processing personal data. You may export text. Deleting a session deletes its Yufolo cloud text, but does not automatically delete data already held by the provider or undo training already performed. Optional local recording copies are stored on your device.',
        },
      ],
    },
    {
      id: 'ai',
      heading: '6. AI features',
      blocks: [
        {
          type: 'p',
          text: 'Translation, answers, summaries, notes, action items, skill maps and practice questions are produced by automated models. They can be wrong, incomplete or stale. They are not legal, medical, academic or other professional advice and do not replace your judgement. You are responsible for any use of an AI output.',
        },
        {
          type: 'p',
          text: 'To keep costs explicit, semantic indexing and most generation tasks require you to start or confirm them. Context preview can run a free lexical preflight by default; a semantic query runs only when you ask for it, and is billed accordingly.',
        },
      ],
    },
    {
      id: 'fees',
      heading: '7. Fees, wallet and membership',
      blocks: [
        {
          type: 'ul',
          items: [
            'The ledger is kept in US dollars. Transcription is billed from forwarded audio bytes converted to duration; AI is billed by tokens or the published rule. A call may reserve an estimated maximum, then settle to actual use and refund the unused reserve. Online payments are charged in the currency and at the exchange rate shown at checkout (the official service currently charges in Australian dollars); balances and charges stay in US dollars, and the checkout rate applies to that payment only.',
            'Grant credit (including a sign-up trial) may expire. Purchased wallet credit usually does not expire while the account exists, but it is not cash you can withdraw.',
            'Membership provides the prices, usage discounts and applicable limits expressly shown for the plan at purchase. It does not include unlimited transcription time or make features already available to ordinary users exclusive to members. Subscriptions bill on the displayed cycle and may be cancelled through the Stripe customer portal or in-product instructions. The end of paid access follows the checkout information and subscription status.',
            'Refund requests are reviewed manually. Contact support@coyume.com from your registered email with the order or transaction details and reason. Properly delivered and consumed usage is non-refundable. Unused purchased credit is reviewed and refunded under these rules: the refund is capped at the amount actually paid for that top-up, less all usage consumed since that top-up (whether it was drawn from promotional credit or from the wallet first), and is returned to the original payment method in the original currency and amount, unchanged by later exchange-rate movements; any bonus credit granted with that top-up lapses on refund and is never converted to cash. Subscription fees are reviewed against the order, usage and applicable law. Promotional credit cannot be exchanged for cash. Incorrect or duplicate charges, non-delivery, service defects and other legally required refunds are not excluded by the non-refund rule; manual review does not remove statutory rights. Cancelling a subscription does not itself submit a refund request. Where we close an account under section 3, or you leave because you do not accept a change under section 13, unused purchased credit is refunded as those sections describe.',
            'Training programme discount: on deployments that offer the programme, members of it receive a discount on transcription charges (not on add-ons such as translation), stacked with any membership discount (the membership discount first, then the programme discount on the remainder), from recordings that start after joining; after leaving, the standard price applies. The rate is the one shown in onboarding, Settings and the pricing section when you choose; the discount is a price term and section 13 governs changes to it.',
            'Credit granted through promotions, invitations or campaigns does not earn the training programme discount: it is billed at the standard price and processed through the account without training. Paid balance (including top-up bonuses) is billed according to your programme choice. Gift credit is spent before paid balance; when it runs out during a session, that session keeps its account and price, and afterwards the extra charged on the paid part is returned to your wallet according to your choice and discount rates at the start of that session and shown in your statement. The programme is optional and we may pause or reopen it; a pause removes everyone from it and reopening requires joining again; changes affect later sessions only. Institution accounts are always processed through the account without training.',
            'Prices, markup, plans and top-up tiers may change. Completed settlements use the price in force at the time.',
            'If the balance cannot cover a reservation, the feature may be refused until you add credit.',
            'Automatic top-up: members can turn on automatic top-up in the account panel and set the trigger threshold and the amount of each top-up. Once on, when a usage reservation fails because the balance is below your threshold, we charge the payment method saved with Stripe for the amount you set and credit it to your wallet; every charge appears in your ledger. It is off by default, only you can turn it on, and you can turn it off in the account panel at any time; after that no further automatic charges are made and credit already added follows the rules in this section. If a charge fails, the feature stays refused until you top up manually.',
          ],
        },
        {
          type: 'p',
          text: 'These terms do not exclude non-excludable consumer rights under applicable law.',
        },
      ],
    },
    {
      id: 'ip',
      heading: '8. Our intellectual property',
      blocks: [
        {
          type: 'p',
          text: 'The Yufolo / DreamTrans software, interface, marks, copy and documentation belong to Coyume Pty Ltd or its licensors. The software is offered under the PolyForm Noncommercial License 1.0.0 and the licence files in the repository; unauthorised commercial use may be infringement. These terms give you a limited, non-transferable, revocable licence to use the official service. They do not transfer any intellectual property.',
        },
      ],
    },
    {
      id: 'thirdparty',
      heading: '9. Third-party services',
      blocks: [
        {
          type: 'p',
          text: 'Speech recognition, model APIs, payments and email are provided by third parties. You may also be bound by their terms. Within the limits of the law we are not liable for outages, refusals, price changes or policy changes at those providers, though we will take reasonable steps to tell you when a feature is affected.',
        },
      ],
    },
    {
      id: 'disclaimer',
      heading: '10. Disclaimer',
      blocks: [
        {
          type: 'p',
          text: 'To the extent permitted by law, the service is provided “as is” and “as available”. We do not warrant that it will be uninterrupted or error-free, or that transcription, translation or AI output is fit for a particular purpose. Live recognition needs the network and third parties. Confirmed text may remain on the device after a drop, but that is not offline recognition.',
        },
      ],
    },
    {
      id: 'liability',
      heading: '11. Limitation of liability',
      blocks: [
        {
          type: 'p',
          text: 'This section does not limit liability that applicable law prohibits us from excluding or limiting. To the extent permitted by law, we are not liable for indirect or consequential loss. For liability that may lawfully be limited, our aggregate liability arising from the same event or related events is capped at the purchased wallet credit you actually consumed for the affected service in the calendar month in which the event occurred, plus the relevant subscription fees attributable to that month. Amounts are not excluded merely because the wallet top-up or subscription payment occurred in an earlier month; promotional credit is excluded.',
        },
        {
          type: 'p',
          text: 'If a third party brings a claim against us because you breached these terms, recorded unlawfully or infringed their rights, you will compensate us for the reasonable losses, costs and reasonable legal fees we incur, to the extent the claim was directly caused by your conduct. Your share is reduced to the extent the loss was caused by our own breach, negligence or unlawful act. We will notify you of the claim promptly and cooperate reasonably in its defence. This paragraph applies only to the extent permitted by law and does not reduce your statutory rights as a consumer.',
        },
      ],
    },
    {
      id: 'term',
      heading: '12. Term and termination',
      blocks: [
        {
          type: 'p',
          text: 'You may stop using the service and delete sessions at any time. Account deletion is handled on request — write to support@coyume.com. We may suspend the service immediately for maintenance, security, unlawfulness or a serious breach. Clauses that by their nature should survive (including fees, intellectual property, disclaimers and liability limits) continue after termination.',
        },
      ],
    },
    {
      id: 'changes',
      heading: '13. Changes',
      blocks: [
        {
          type: 'p',
          text: 'We may update these terms for legal, security, product or business reasons and will post the new version and its effective date on this page. If a change materially affects your rights or pricing, we will give you at least 30 days’ notice on the site or by email to your registered address; changes required by law or urgent security needs may take effect sooner. If you do not accept a change, you may stop using the service before the effective date and write to support@coyume.com to close your account: we will refund unused purchased credit and a pro-rata share of any current subscription period, and promotional credit lapses. Using the service after the effective date means you accept the updated terms, without affecting that exit right or your non-excludable statutory rights. Price changes follow these rules: a change that applies only to new top-ups and new subscriptions takes effect when published; a usage-rate increase that would make existing purchased credit buy less usage is not a material adverse change to that credit, and may take effect when published, if at the switch we top up existing balances by the same proportion so their usable amount is not reduced, and we will tell you at the same time; any other change affecting purchased credit or the renewal price of an existing subscription is subject to the 30 days’ notice above.',
        },
      ],
    },
    {
      id: 'law',
      heading: '14. Governing law',
      blocks: [
        {
          type: 'p',
          text: 'These terms are governed by the laws of Australia. Where mandatory consumer jurisdiction rules allow, the courts of Australia have jurisdiction. If one clause cannot be enforced, the rest remains in force.',
        },
      ],
    },
    {
      id: 'contact',
      heading: '15. Contact',
      blocks: [
        {
          type: 'p',
          text: 'Questions about these terms can be sent to support@coyume.com. The official Yufolo service is operated by Coyume Pty Ltd in Australia.',
        },
      ],
    },
  ],
}

const DOCUMENTS: Record<LegalKind, Record<Locale, LegalDocument>> = {
  privacy: { 'zh-CN': privacyZh, en: privacyEn },
  terms: { 'zh-CN': termsZh, en: termsEn },
}

export function legalDocument(kind: LegalKind, locale: Locale): LegalDocument {
  return DOCUMENTS[kind][locale]
}
