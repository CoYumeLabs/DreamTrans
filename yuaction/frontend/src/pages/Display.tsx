import { useMemo } from "react";
import { captionFeed } from "../captionFeed";
import { MessageCircle, Radio } from "lucide-react";
import { QRCodeSVG } from "qrcode.react";
import { useRoom } from "../useRoom";
import { Brand, Connection, ErrorNote, Pill } from "../components/ui";
import { useMessages } from "../i18n";
import { LocaleSwitch } from "../i18n/LocaleSwitch";

export default function Display({ code }: { code: string }) {
  const m = useMessages();
  const { room, connection, error } = useRoom(code);
  const question = room?.questions.find((q) => q.status === "showing");
  const segment = useMemo(
    () => captionFeed(room?.segments || []).at(-1),
    [room?.segments],
  );
  const url = `${window.location.origin}/rooms/${code}`;
  return (
    <div className="display-page">
      <header>
        <Brand light />
        <div className="display-tools">
          <LocaleSwitch />
          <Connection state={connection} />
        </div>
      </header>
      <main>
        <div className="display-eyebrow">
          {room?.title || m.display.connecting}
        </div>
        <ErrorNote message={error} />
        {room?.status === "ended" ? (
          <>
            <Pill>{m.display.ended}</Pill>
            <h1>
              {m.display.thanks}
              <br />
              {m.display.thanksLine}
            </h1>
          </>
        ) : question ? (
          <>
            <span className="display-label">
              <MessageCircle size={20} />
              {m.display.discuss}
            </span>
            <h1 className="display-question">{question.content}</h1>
          </>
        ) : (
          <>
            <span className="display-label">
              <Radio size={20} />
              {m.display.listen}
            </span>
            <h1>
              {m.display.start}
              <br />
              {m.display.startLine}
            </h1>
            <p>{m.display.scan}</p>
          </>
        )}
        {segment && (
          <div className="display-caption">
            {segment.source === "demo" && (
              <span className="display-demo">{m.display.demo}</span>
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
          <small>{m.display.joinHint}</small>
        </div>
        <div className="display-qr">
          <QRCodeSVG value={url} size={112} level="M" />
        </div>
      </footer>
    </div>
  );
}
