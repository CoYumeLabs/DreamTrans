import Home from "./pages/Home";
import Host from "./pages/Host";
import Display from "./pages/Display";
import RoomPage from "./pages/RoomPage";
import { Brand } from "./components/ui";

export default function App() {
  const match = window.location.pathname.match(
    /^\/rooms\/([a-fA-F0-9]{8})(?:\/(host|display))?\/?$/,
  );
  if (match) {
    const code = match[1].toUpperCase();
    return match[2] === "host" ? (
      <Host code={code} />
    ) : match[2] === "display" ? (
      <Display code={code} />
    ) : (
      <RoomPage code={code} />
    );
  }
  if (window.location.pathname !== "/")
    return (
      <div className="center-page">
        <Brand />
        <h1>没有找到这个页面</h1>
        <a href="/">返回活动空间</a>
      </div>
    );
  return <Home />;
}
