// Shared API loaders that populate the console store.
import { api } from "./api.js";
import * as store from "./store.js";
import { toast } from "./ui.js";

export async function refreshMonitor() {
  try {
    store.set("monitor", await api("/api/monitor"));
  } catch {
    // The 3s poll stays silent; transient failures surface in the log stream.
  }
}

export async function loadConfig() {
  store.set("config", await api("/api/config"));
}

export async function loadDebugModels() {
  try {
    const data = await api("/api/debug/models");
    if (data.last_inference) store.set("lastPlayground", data.last_inference);
    store.set("debug", data);
  } catch (error) {
    toast(error.message, true);
  }
}
