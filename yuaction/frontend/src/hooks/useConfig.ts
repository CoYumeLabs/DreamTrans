import { useEffect, useState } from "react";
import { api, type Config } from "../api";

export function useConfig() {
  const [config, setConfig] = useState<Config | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    api<Config>("/config")
      .then(setConfig)
      .catch((e) => setError(e.message));
  }, []);
  return { config, error };
}
