// Page: 配置中心 — masked config editing with validation, hot reload and reveal.
import { api, hasSession } from "../api.js";
import * as store from "../store.js";
import { t, onChange } from "../i18n.js";
import { $, $$, bindTabs, el, openDialog, toast } from "../ui.js";
import { fillProxies, initProxies, lockProxies, proxyInputs, resetProxies, stopChecks } from "./proxies.js";

// A long key list is rendered collapsed: the first CHIP_RENDER_LIMIT chips
// plus a toggle. A single flex-wrap row with thousands of chips is what froze
// the config page on large pools.
const CHIP_RENDER_LIMIT = 100;
const expandedChips = new Set();
const removedChips = new Map();
let mutation = null;
let revealOperation = null;

function renderChips(name) {
  const host = $(`#${name}-chips`);
  host.replaceChildren();
  const items = (store.get("config")?.[name] || []).filter((item) => !removedChips.get(name)?.has(item.id));
  const expanded = expandedChips.has(name);
  const visible = expanded ? items : items.slice(0, CHIP_RENDER_LIMIT);
  visible.forEach((item) => {
    const remove = el("button", { type: "button", title: t("移除", "common.remove"), text: "×" });
    const chip = el("span", { class: "chip", dataset: { id: item.id } });
    remove.addEventListener("click", () => {
      if (!removedChips.has(name)) removedChips.set(name, new Set());
      removedChips.get(name).add(item.id);
      renderChips(name);
    });
    chip.append(el("span", { text: item.display }), remove);
    host.append(chip);
  });
  if (items.length > visible.length)
    host.append(chipToggle(`config.chips.expand`, `展开全部（共 ${items.length} 个）`, items.length, () => {
      expandedChips.add(name);
      renderChips(name);
    }));
  else if (expanded)
    host.append(chipToggle(`config.chips.collapse`, "收起", null, () => {
      expandedChips.delete(name);
      renderChips(name);
    }));
}

function chipToggle(key, fallback, count, onClick) {
  const props = { type: "button", class: "chip chip-toggle", text: t(fallback, key, count) };
  if (count != null) props.dataset = { count };
  const button = el("button", props);
  button.addEventListener("click", onClick);
  return button;
}

function renderRestartNotice(value) {
  const notice = $("#restart-notice");
  if (value?.restart_required_fields?.length) {
    notice.textContent = t(
      `已保存但尚未生效：${value.restart_required_fields.join(", ")}。当前 API ${value.effective.listen}、WebUI ${value.effective.webui_listen}。请重启进程。`,
      "config.restartNotice",
      value.restart_required_fields.join(", "),
      value.effective.listen,
      value.effective.webui_listen,
    );
    notice.classList.remove("hidden");
  } else {
    notice.classList.add("hidden");
  }
}

function secretInputs(name) {
  if (name === "proxies") return proxyInputs();
  // Collapsed chips are only a view; keys outside the rendered page must
  // survive saving, expanding, and collapsing the list.
  const kept = (store.get("config")?.[name] || [])
    .filter((item) => !removedChips.get(name)?.has(item.id))
    .map((item) => ({ id: item.id }));
  const added = $(`#${name}-new`)
    .value.split(/\r?\n/)
    .map((value) => value.trim())
    .filter(Boolean)
    .map((value) => ({ value }));
  return kept.concat(added);
}

const numberValue = (selector) => Number($(selector).value);

function fillConfig(value) {
  if (!value) return;
  $("#c-listen").value = value.listen;
  $("#c-prefer").value = value.prefer;
  $("#c-web-listen").value = value.webui.listen;
  $("#c-session").value = value.webui.session_ttl_minutes;
  $("#c-web-enabled").checked = value.webui.enabled;
  $("#c-anonymous").checked = !!value.anonymous;
  const warp = value.warp || {};
  $("#c-warp-enabled").checked = !!warp.enabled;
  $("#c-warp-pool").value = warp.pool_size || 2;
  $("#c-warp-stack").value = warp.stack || "ipv6";
  $("#c-warp-source").value = warp.source_ipv6 || "";
  $("#c-proxyfile").value = value.proxyfile;
  $("#c-up-zen").value = value.upstream.zen;
  $("#c-up-go").value = value.upstream.go;
  $("#c-attempts").value = value.retry.max_attempts;
  $("#c-timeout").value = value.retry.timeout_seconds;
  $("#c-refresh").value = value.models.refresh_seconds;
  $("#c-protocols").value = JSON.stringify(value.models.protocols || {}, null, 2);
  $("#c-free-only").checked = !!value.models.free_only;
  $("#c-active-probes").checked = !!value.models.active_probes;
  const reasoning = value.reasoning || {};
  $("#c-reasoning-effort").value = reasoning.effort || "";
  $("#c-reasoning-by-model").value = JSON.stringify(reasoning.effort_by_model || {}, null, 2);
  $("#c-idle").value = value.performance.max_idle_conns;
  $("#c-idle-host").value = value.performance.max_idle_conns_per_host;
  $("#c-max-host").value = value.performance.max_conns_per_host;
  $("#c-idle-timeout").value = value.performance.idle_conn_timeout_seconds;
  $("#c-connect").value = value.performance.connect_timeout_seconds;
  $("#c-cooldown").value = value.performance.failure_cooldown_seconds;
  $("#c-attempt-timeout").value = value.performance.attempt_timeout_seconds;
  $("#c-first-event-timeout").value = value.performance.first_event_timeout_seconds || 0;
  $("#c-level").value = value.logging.level;
  $("#c-ring").value = value.logging.ring_size;
  $("#c-dump-bodies").checked = !!value.logging.dump_request_bodies;
  ["server_keys", "zen_keys", "go_keys"].forEach(renderChips);
  fillProxies(value.proxies || []);
  renderRestartNotice(value);
}

function lockConfig(value) {
  lockProxies(value);
  $$("#config-form .sticky-actions button").forEach((button) => (button.disabled = value));
}

function currentMutation(operation) {
  return mutation === operation && hasSession();
}

async function refreshDebug(operation) {
  const value = await api("/api/debug/models", { signal: operation.controller.signal });
  if (!currentMutation(operation)) return;
  if (value.last_inference) store.set("lastPlayground", value.last_inference);
  store.set("debug", value);
}

async function save(event) {
  event.preventDefault();
  const current = store.get("config");
  if (!current || mutation || !hasSession()) return;
  const operation = { controller: new AbortController() };
  mutation = operation;
  lockConfig(true);
  try {
    const update = {
      listen: $("#c-listen").value,
      server_keys: secretInputs("server_keys"),
      zen_keys: secretInputs("zen_keys"),
      go_keys: secretInputs("go_keys"),
      anonymous: $("#c-anonymous").checked,
      warp: {
        enabled: $("#c-warp-enabled").checked,
        pool_size: numberValue("#c-warp-pool"),
        stack: $("#c-warp-stack").value,
        source_ipv6: $("#c-warp-source").value.trim(),
      },
      proxies: secretInputs("proxies"),
      proxyfile: $("#c-proxyfile").value,
      prefer: $("#c-prefer").value,
      upstream: { ...current.upstream, zen: $("#c-up-zen").value, go: $("#c-up-go").value },
      retry: { max_attempts: numberValue("#c-attempts"), timeout_seconds: numberValue("#c-timeout") },
      models: {
        ...current.models,
        refresh_seconds: numberValue("#c-refresh"),
        protocols: JSON.parse($("#c-protocols").value || "{}"),
        free_only: $("#c-free-only").checked,
        active_probes: $("#c-active-probes").checked,
      },
      reasoning: {
        effort: $("#c-reasoning-effort").value,
        effort_by_model: JSON.parse($("#c-reasoning-by-model").value || "{}"),
      },
      performance: {
        ...current.performance,
        max_idle_conns: numberValue("#c-idle"),
        max_idle_conns_per_host: numberValue("#c-idle-host"),
        max_conns_per_host: numberValue("#c-max-host"),
        idle_conn_timeout_seconds: numberValue("#c-idle-timeout"),
        connect_timeout_seconds: numberValue("#c-connect"),
        failure_cooldown_seconds: numberValue("#c-cooldown"),
        attempt_timeout_seconds: numberValue("#c-attempt-timeout"),
        first_event_timeout_seconds: numberValue("#c-first-event-timeout"),
      },
      logging: {
        level: $("#c-level").value,
        ring_size: numberValue("#c-ring"),
        dump_request_bodies: $("#c-dump-bodies").checked,
      },
      webui: {
        enabled: $("#c-web-enabled").checked,
        listen: $("#c-web-listen").value,
        username: current.webui.username,
        session_ttl_minutes: numberValue("#c-session"),
      },
    };
    const result = await api("/api/config", {
      method: "PUT", body: JSON.stringify(update), signal: operation.controller.signal,
    });
    if (!currentMutation(operation)) return;
    fillProxies(result.config.proxies || [], true);
    removedChips.clear();
    store.set("config", result.config);
    ["server_keys", "zen_keys", "go_keys"].forEach(
      (name) => ($(`#${name}-new`).value = ""),
    );
    await refreshDebug(operation);
    if (!currentMutation(operation)) return;
    toast(
      result.result.restart_required
        ? t(
            `已应用；${result.result.restart_fields.join(", ")} 需重启`,
            "toast.config.restart",
            result.result.restart_fields.join(", "),
          )
        : t("配置已保存并立即应用", "toast.config.saved"),
    );
  } catch (error) {
    if (currentMutation(operation) && error.name !== "AbortError") toast(error.message, true);
  } finally {
    if (mutation === operation) {
      mutation = null;
      lockConfig(false);
    }
  }
}

async function reload() {
  if (mutation || !hasSession()) return;
  const operation = { controller: new AbortController() };
  mutation = operation;
  lockConfig(true);
  try {
    const result = await api("/api/config/reload", {
      method: "POST", body: "{}", signal: operation.controller.signal,
    });
    if (!currentMutation(operation)) return;
    const value = await api("/api/config", { signal: operation.controller.signal });
    if (!currentMutation(operation)) return;
    fillProxies(value.proxies || [], true);
    removedChips.clear();
    store.set("config", value);
    await refreshDebug(operation);
    if (!currentMutation(operation)) return;
    toast(
      result.restart_required
        ? t("磁盘配置已重载，监听字段需重启", "toast.config.reloadedRestart")
        : t("磁盘配置已重载", "toast.config.reloaded"),
    );
  } catch (error) {
    if (currentMutation(operation) && error.name !== "AbortError") toast(error.message, true);
  } finally {
    if (mutation === operation) {
      mutation = null;
      lockConfig(false);
    }
  }
}

function reveal() {
  if (revealOperation || !hasSession()) return;
  const operation = { controller: new AbortController(), busy: false };
  revealOperation = operation;
  const input = el("input", { type: "password", autocomplete: "current-password" });
  const submit = async (close) => {
    if (revealOperation !== operation || operation.busy || !hasSession()) return;
    operation.busy = true;
    try {
      const values = await api("/api/config/reveal", {
        method: "POST",
        body: JSON.stringify({ password: input.value }),
        signal: operation.controller.signal,
      });
      if (revealOperation !== operation || !hasSession()) return;
      close();
      openDialog({
        title: t("敏感配置", "modal.title"),
        size: "dialog-lg",
        body: el("pre", { text: JSON.stringify(values, null, 2) }),
        actions: [{ label: t("关闭", "modal.close") }],
      });
    } catch (error) {
      if (revealOperation === operation && error.name !== "AbortError") toast(error.message, true);
    } finally {
      operation.busy = false;
    }
  };
  const dialog = openDialog({
    title: t("查看敏感配置", "modal.revealTitle"),
    body: el("div", { class: "field" }, [
      el("label", { text: t("请再次输入管理密码", "config.revealPrompt") }),
      input,
    ]),
    onClose: () => {
      input.value = "";
      operation.controller.abort();
      if (revealOperation === operation) revealOperation = null;
    },
    actions: [
      { label: t("取消", "common.cancel") },
      { label: t("查看", "common.view"), kind: "btn-primary", onClick: submit },
    ],
  });
  input.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      submit(dialog.close);
    }
  });
}

export default {
  id: "config",
  title: "配置中心",
  sub: "验证后原子保存，并热应用可变字段。",
  hide: stopChecks,
  reset() {
    mutation?.controller.abort();
    mutation = null;
    revealOperation?.controller.abort();
    revealOperation = null;
    removedChips.clear();
    expandedChips.clear();
    ["server_keys", "zen_keys", "go_keys"].forEach((name) => ($(`#${name}-new`).value = ""));
    lockConfig(false);
    resetProxies();
  },
  init() {
    store.on("config", fillConfig);
    initProxies();
    bindTabs($("#page-config"), (id) => { if (id !== "proxies") stopChecks(); });
    $("#config-form").addEventListener("submit", save);
    $("#reload-config").addEventListener("click", reload);
    $("#reveal").addEventListener("click", reveal);
    onChange(() => {
      const value = store.get("config");
      if (value) renderRestartNotice(value);
      // Update chip tooltips in place so pending unsaved chip removals survive a language switch.
      $$(".chips .chip > button").forEach((button) => (button.title = t("移除", "common.remove")));
      $$(".chip-toggle").forEach((node) => {
        node.textContent = node.dataset.count
          ? t(`展开全部（共 ${node.dataset.count} 个）`, "config.chips.expand", node.dataset.count)
          : t("收起", "config.chips.collapse");
      });
    });
  },
};
