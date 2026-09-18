import { useState } from "react";
import { Check, Copy, Users } from "lucide-react";
import { QRCodeSVG } from "qrcode.react";
import type { Room } from "../api";
import { Button, ErrorNote } from "./ui";

export default function Share({ room }: { room: Room }) {
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState("");
  const url = `${window.location.origin}/rooms/${room.code}`;
  async function copy() {
    try {
      await navigator.clipboard.writeText(url);
      setCopied(true);
    } catch {
      setError("请手动复制下方链接");
    }
  }
  return (
    <section className="panel share-panel">
      <div className="panel-heading">
        <h3>邀请大家加入</h3>
        <Users size={18} />
      </div>
      <div className="share-layout">
        <div className="qr-frame">
          <QRCodeSVG value={url} size={104} level="M" />
        </div>
        <div>
          <span className="muted-label">扫码，或输入房间码</span>
          <strong className="room-code">{room.code}</strong>
          <Button className="soft small" onClick={copy}>
            {copied ? <Check size={15} /> : <Copy size={15} />}
            {copied ? "链接已复制" : "复制邀请链接"}
          </Button>
        </div>
      </div>
      <input
        className="share-url"
        aria-label="邀请链接"
        readOnly
        value={url}
        onFocus={(e) => e.target.select()}
      />
      {window.location.hostname === "localhost" ||
      window.location.hostname === "127.0.0.1" ? (
        <p className="form-note">手机扫码需使用可访问的局域网地址打开本页。</p>
      ) : null}
      <ErrorNote message={error} />
    </section>
  );
}
