import { useState } from "react";
import { Check, Copy, Users } from "lucide-react";
import { QRCodeSVG } from "qrcode.react";
import type { Room } from "../api";
import { Button, ErrorNote } from "./ui";
import { useMessages } from "../i18n";

export default function Share({ room }: { room: Room }) {
  const m = useMessages();
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState("");
  const url = `${window.location.origin}/rooms/${room.code}`;
  async function copy() {
    try {
      await navigator.clipboard.writeText(url);
      setCopied(true);
    } catch {
      setError(m.share.copyFailed);
    }
  }
  return (
    <section className="panel share-panel">
      <div className="panel-heading">
        <h3>{m.share.title}</h3>
        <Users size={18} />
      </div>
      <div className="share-layout">
        <div className="qr-frame">
          <QRCodeSVG value={url} size={104} level="M" />
        </div>
        <div>
          <span className="muted-label">{m.share.hint}</span>
          <strong className="room-code">{room.code}</strong>
          <Button className="soft small" onClick={copy}>
            {copied ? <Check size={15} /> : <Copy size={15} />}
            {copied ? m.share.copied : m.share.copy}
          </Button>
        </div>
      </div>
      <input
        className="share-url"
        aria-label={m.share.link}
        readOnly
        value={url}
        onFocus={(e) => e.target.select()}
      />
      {window.location.hostname === "localhost" ||
      window.location.hostname === "127.0.0.1" ? (
        <p className="form-note">{m.share.localhost}</p>
      ) : null}
      <ErrorNote message={error} />
    </section>
  );
}
