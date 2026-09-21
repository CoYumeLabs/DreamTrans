import Home from "./pages/Home";
import Host from "./pages/Host";
import Display from "./pages/Display";
import RoomPage from "./pages/RoomPage";
import { Brand } from "./components/ui";
import { LocaleSwitch } from "./i18n/LocaleSwitch";
import { useMessages } from "./i18n";

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
  if (window.location.pathname !== "/") return <NotFound />;
  return <Home />;
}

function NotFound() {
  const m = useMessages();
  return (
    <div className="center-page">
      <LocaleSwitch />
      <Brand />
      <h1>{m.notFound.title}</h1>
      <a href="/">{m.notFound.back}</a>
    </div>
  );
}
