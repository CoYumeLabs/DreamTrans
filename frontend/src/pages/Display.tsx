import { MessageCircle, Radio } from "lucide-react";
import { QRCodeSVG } from "qrcode.react";
import { useRoom } from "../useRoom";
import { Brand, Connection, ErrorNote, Pill } from "../components/ui";

export default function Display({ code }: { code: string }) {
  const { room, connection, error } = useRoom(code);
  const question = room?.questions.find((q) => q.status === "showing");
  const segment = room?.segments.at(-1);
  const url = `${window.location.origin}/rooms/${code}`;
  return (
    <div className="display-page">
      <header>
        <Brand light />
        <Connection state={connection} />
      </header>
      <main>
        <div className="display-eyebrow">{room?.title || "正在连接房间"}</div>
        <ErrorNote message={error} />
        {room?.status === "ended" ? (
          <>
            <Pill>活动已结束</Pill>
            <h1>
              谢谢每一次提问，
              <br />
              和每一个认真倾听的你。
            </h1>
          </>
        ) : question ? (
          <>
            <span className="display-label">
              <MessageCircle size={20} />
              现在，让我们聊聊
            </span>
            <h1 className="display-question">{question.content}</h1>
          </>
        ) : (
          <>
            <span className="display-label">
              <Radio size={20} />
              让每个声音，都被听见
            </span>
            <h1>
              好的交流，
              <br />
              从一个问题开始。
            </h1>
            <p>扫码加入，分享你的疑问与想法。</p>
          </>
        )}
        {segment && (
          <div className="display-caption">
            {segment.source === "demo" && (
              <span className="display-demo">演示字幕</span>
            )}
            <p>{segment.text}</p>
            {segment.translation && (
              <p className="translation">{segment.translation}</p>
            )}
          </div>
        )}
      </main>
      <footer>
        <div>
          <span>JOIN THE CONVERSATION</span>
          <strong>{code}</strong>
          <small>打开 YuAction，输入房间码</small>
        </div>
        <div className="display-qr">
          <QRCodeSVG value={url} size={112} level="M" />
        </div>
      </footer>
    </div>
  );
}
