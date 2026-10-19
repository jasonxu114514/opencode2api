// Console shell: boot, login, navigation and shared data loading.
import { api, setCsrf, clearCsrf, setUnauthorizedHandler } from "./api.js";
import { refreshMonitor, loadConfig, loadDebugModels } from "./data.js";
import * as i18n from "./i18n.js";
import * as store from "./store.js";
import { $, $$, toast } from "./ui.js";
import * as logstream from "./logstream.js";

import overview from "./pages/overview.js";
import diagnostics from "./pages/diagnostics.js";
import access from "./pages/access.js";
import apiKeys from "./pages/api-keys.js";
import quotas from "./pages/model-quotas.js";
import rotation from "./pages/rotation.js";
import configPage from "./pages/config.js";
import account from "./pages/account.js";

const PAGES = [overview, diagnostics, access, apiKeys, quotas, rotation, configPage, account];
const byId = new Map(PAGES.map((page) => [page.id, page]));

// Hrefs from the previous 13-page layout keep working.
const HASH_ALIASES = {
  guide: "overview",
  usage: "overview",
  playground: "diagnostics",
  logs: "diagnostics",
  availability: "model-quotas",
};

let current = null;
let monitorTimer = null;

function renderPageHeader(page) {
  if (!page) return;
  $("#page-title").textContent = i18n.t(page.title, `page.${page.id}.title`);
  $("#page-sub").textContent = i18n.t(page.sub, `page.${page.id}.sub`);
}

function openPage(id) {
  const page = byId.get(id);
  if (!page) return;
  current?.hide?.();
  history.replaceState(null, "", `#${id}`);
  $$(".nav-item").forEach((button) =>
    button.classList.toggle("active", button.dataset.page === id),
  );
  $$(".page").forEach((node) => node.classList.toggle("hidden", node.id !== `page-${id}`));
  renderPageHeader(page);
  current = page;
  page.show?.();
}

function showLogin() {
  clearCsrf();
  current?.hide?.();
  current = null;
  $("#app").classList.add("hidden");
  $("#login").classList.remove("hidden");
  clearInterval(monitorTimer);
  monitorTimer = null;
  logstream.disconnect();
  for (const page of PAGES) page.reset?.();
}

async function enterConsole(session) {
  setCsrf(session.csrf_token);
  $("#login").classList.add("hidden");
  $("#app").classList.remove("hidden");
  await loadConfig();
  await Promise.all([refreshMonitor(), loadDebugModels()]);
  clearInterval(monitorTimer);
  monitorTimer = setInterval(refreshMonitor, 3000);
  logstream.connect();
  const hash = location.hash.slice(1);
  const id = byId.has(hash) ? hash : HASH_ALIASES[hash];
  openPage(id && byId.has(id) ? id : "overview");
}

async function boot() {
  try {
    await enterConsole(await api("/api/auth/session"));
  } catch {
    showLogin();
  }
}

setUnauthorizedHandler(showLogin);

store.on("monitor", (data) => {
  const version = String(data.version || "");
  $("#version").textContent = version.startsWith("v") || version === "dev" ? version : `v${version}`;
});

for (const page of PAGES) page.init?.();

$$(".nav-item").forEach((button) => {
  button.addEventListener("click", () => openPage(button.dataset.page));
});

$("#refresh-now").addEventListener("click", async () => {
  await Promise.all([refreshMonitor(), loadConfig(), loadDebugModels()]);
  await current?.refresh?.();
  toast(i18n.t("状态已刷新", "toast.refreshed"));
});

$("#logout").addEventListener("click", async () => {
  try {
    await api("/api/auth/logout", { method: "POST", body: "{}" });
  } finally {
    showLogin();
  }
});

$("#login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  $("#login-error").textContent = "";
  try {
    const session = await api("/api/auth/login", {
      method: "POST",
      body: JSON.stringify({ username: $("#login-user").value, password: $("#login-pass").value }),
    });
    $("#login-pass").value = "";
    await enterConsole(session);
  } catch (error) {
    $("#login-error").textContent = error.message;
  }
});

i18n.onChange(() => {
  renderPageHeader(current);
  logstream.syncPauseButton();
  logstream.render();
});

i18n.init();
boot();
