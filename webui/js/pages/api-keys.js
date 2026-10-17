// Page: API Key 管理 — create, rename, enable/disable and delete caller keys.
import { api, derivedAPIBase, hasSession } from "../api.js";
import { loadConfig } from "../data.js";
import * as store from "../store.js";
import { t, onChange } from "../i18n.js";
import { $, el, emptyRow, formatNumber, localTime, confirmDialog, copyText, promptDialog, toast } from "../ui.js";

let keysLoading = false;
let createdKey = "";
let lastKeys = null;

async function load() {
  if (keysLoading) return;
  keysLoading = true;
  $("#api-key-refresh").disabled = true;
  try {
    const data = await api("/api/api-keys");
    if (hasSession()) {
      lastKeys = data.keys;
      render(data.keys);
    }
  } catch (error) {
    $("#api-key-status").textContent = t(
      `读取失败：${error.message}`,
      "apikeys.loadFailed",
      error.message,
    );
  } finally {
    keysLoading = false;
    $("#api-key-refresh").disabled = false;
  }
}

async function mutate(id, method, body, button) {
  button.disabled = true;
  try {
    const data = await api(`/api/api-keys/${encodeURIComponent(id)}`, {
      method,
      ...(body ? { body: JSON.stringify(body) } : {}),
    });
    if (!hasSession()) return;
    lastKeys = data.keys;
    render(data.keys);
    await loadConfig();
    toast(t("已保存并立即生效", "toast.apikeys.saved"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function render(keys) {
  keys = keys || [];
  $("#api-key-count").textContent = t(`${keys.length} 个`, "apikeys.count", keys.length);
  $("#api-key-status").textContent = t(
    `${keys.filter((key) => key.enabled).length} 个已启用`,
    "apikeys.enabledCount",
    keys.filter((key) => key.enabled).length,
  );
  const host = $("#api-key-list");
  host.replaceChildren();
  for (const key of keys) {
    const usage = key.usage || {};
    const summary = (value = {}) =>
      `${formatNumber(value.requests)} ${t("次", "apikeys.times")} · ${formatNumber(value.total_tokens)} tokens`;
    const rename = el("button", { type: "button", class: "btn btn-sm btn-ghost", text: t("重命名", "common.rename") });
    rename.addEventListener("click", async () => {
      const name = await promptDialog({
        title: t("重命名 API Key", "common.rename"),
        label: t("密钥名称", "apikeys.namePrompt"),
        value: key.name,
        confirmLabel: t("保存", "common.save"),
      });
      if (name !== null) mutate(key.id, "PATCH", { name }, rename);
    });
    const toggle = el("button", {
      type: "button",
      class: "btn btn-sm btn-ghost",
      text: key.enabled ? t("停用", "apikeys.disable") : t("启用", "apikeys.enable"),
    });
    toggle.addEventListener("click", () => mutate(key.id, "PATCH", { enabled: !key.enabled }, toggle));
    const remove = el("button", { type: "button", class: "btn btn-sm btn-danger", text: t("删除", "common.delete") });
    remove.addEventListener("click", async () => {
      const ok = await confirmDialog({
        title: t("删除 API Key", "common.delete"),
        message: t(
          `删除「${key.name}」后，使用它的应用将无法继续访问。确定删除？`,
          "apikeys.confirmDelete",
          key.name,
        ),
        confirmLabel: t("删除", "common.delete"),
        danger: true,
      });
      if (ok) mutate(key.id, "DELETE", null, remove);
    });
    host.append(
      el("tr", {}, [
        el("td", { text: key.name }),
        el("td", { class: "mono", text: key.display }),
        el("td", {
          class: `status ${key.enabled ? "ok" : "warn"}`,
          text: key.enabled ? t("已启用", "state.enabled") : t("已停用", "state.disabled"),
        }),
        el("td", { text: `${summary(usage.last_hour)} / ${summary(usage.lifetime)}` }),
        el("td", { text: key.created_at ? localTime(key.created_at) : t("已有配置", "apikeys.col.legacy") }),
        el("td", {}, [el("div", { class: "key-actions" }, [rename, toggle, remove])]),
      ]),
    );
  }
  if (!keys.length) emptyRow(host, 6, t("暂无 API Key", "apikeys.empty"));
}

async function create(event) {
  event.preventDefault();
  const button = $("#api-key-create");
  button.disabled = true;
  try {
    const data = await api("/api/api-keys", {
      method: "POST",
      body: JSON.stringify({ name: $("#api-key-name").value, value: $("#api-key-value").value }),
    });
    if (!hasSession()) return;
    createdKey = data.key;
    const goModels = store.get("debug")?.models?.filter((item) => item.available_go) || [];
    const model =
      goModels.find((item) => item.model === "glm-5.3-flash")?.model ||
      goModels[0]?.model ||
      "YOUR_MODEL";
    $("#api-key-env").textContent = `OPENAI_API_KEY=${data.key}\nOPENAI_BASE_URL=${derivedAPIBase()}/v1\nOPENAI_MODEL=${model}`;
    $("#api-key-created").classList.remove("hidden");
    $("#api-key-form").reset();
    lastKeys = data.keys;
    render(data.keys);
    await loadConfig();
    toast(t("API Key 已创建", "toast.apikeys.created"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function resetCreated() {
  createdKey = "";
  lastKeys = null;
  $("#api-key-env").textContent = "";
  $("#api-key-created").classList.add("hidden");
  $("#api-key-value").value = "";
}

export default {
  id: "api-keys",
  title: "API Key 管理",
  sub: "给每个调用方独立的访问密钥。",
  init() {
    $("#api-key-form").addEventListener("submit", create);
    $("#api-key-refresh").addEventListener("click", load);
    $("#api-key-dismiss").addEventListener("click", resetCreated);
    $("#api-key-copy").addEventListener("click", () => copyText(createdKey));
    $("#api-key-copy-env").addEventListener("click", () => copyText($("#api-key-env").textContent));
    // Re-render the rows in place so the key that was just created stays on
    // screen instead of being wiped by a reload.
    onChange(() => {
      if (lastKeys) render(lastKeys);
    });
  },
  show: load,
  refresh: load,
  reset: resetCreated,
};
