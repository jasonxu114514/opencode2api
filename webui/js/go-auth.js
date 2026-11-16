import { api, hasSession } from "./api.js";
import * as store from "./store.js";
import { t, onChange } from "./i18n.js";
import { $, confirmDialog, el, localTime } from "./ui.js";

let state = null;
let timer = null;
let active = false;
let busy = false;
let generation = 0;
let controller = null;
let failure = "";
let connectionChanged = null;
let synchronizationPending = false;

function stopPolling() {
  clearTimeout(timer);
  timer = null;
}

function verificationURL(value) {
  try {
    const parsed = new URL(value, "https://opencode.ai/console/");
    if (
      parsed.protocol !== "https:" ||
      parsed.host !== "opencode.ai" ||
      parsed.username ||
      parsed.password
    )
      return null;
    return parsed.href;
  } catch {
    return null;
  }
}

function pendingExpired() {
  const expires = Date.parse(state?.pending?.expires_at);
  return Number.isFinite(expires) && expires <= Date.now();
}

function connectionID(value) {
  return value?.connected
    ? JSON.stringify([
        value.account?.email,
        value.account?.organization,
        value.account?.models,
        !!value.account?.needs_login,
      ])
    : "";
}

function current(operation) {
  return operation === generation && hasSession();
}

async function synchronize(operation) {
  const [config, debug] = await Promise.all([api("/api/config"), api("/api/debug/models")]);
  if (!current(operation)) return;
  store.set("config", config);
  if (debug.last_inference) store.set("lastPlayground", debug.last_inference);
  store.set("debug", debug);
  await connectionChanged?.();
  if (current(operation)) synchronizationPending = false;
}

function schedulePoll() {
  stopPolling();
  const pending = state?.pending;
  if (
    !active ||
    document.hidden ||
    busy ||
    failure ||
    !hasSession() ||
    !pending ||
    pendingExpired() ||
    pending.status === "failed" ||
    pending.status === "choosing_org"
  )
    return;
  const seconds = Number(pending.retry_after_seconds ?? pending.interval_seconds ?? 5);
  const wait = Number.isFinite(seconds) ? Math.max(1, Math.min(seconds, 86400)) : 5;
  timer = setTimeout(() => {
    if (pendingExpired() || state?.pending?.status === "starting") refresh();
    else poll();
  }, wait * 1000);
}

async function request(path, options = {}, action = "refresh") {
  if (busy || !hasSession()) return;
  stopPolling();
  const operation = generation;
  const previous = connectionID(state);
  busy = true;
  failure = "";
  controller = new AbortController();
  render();
  try {
    const next = await api(path, { ...options, signal: controller.signal });
    if (!current(operation)) return;
    state = next;
    render();
    if (
      action === "disconnect" ||
      previous !== connectionID(next) ||
      (action === "poll" && next.connected && !next.pending)
    )
      synchronizationPending = true;
    if (synchronizationPending) await synchronize(operation);
  } catch (error) {
    if (current(operation) && error.name !== "AbortError") failure = error.message;
  } finally {
    if (current(operation)) {
      busy = false;
      controller = null;
      render();
      schedulePoll();
    }
  }
}

function poll(organizationID) {
  if (!state?.pending?.id) return;
  const body = { id: state.pending.id };
  if (organizationID) body.organization_id = organizationID;
  return request("/api/go/auth/poll", { method: "POST", body: JSON.stringify(body) }, "poll");
}

async function disconnect() {
  const operation = generation;
  const confirmed = await confirmDialog({
    title: t("断开 Go 订阅登录", "goAuth.disconnect"),
    message: t(
      "仅移除网关保存的订阅登录，不删除手动 Go Key，也不会撤销 OpenCode 官方会话。是否继续？",
      "goAuth.disconnectConfirm",
    ),
    confirmLabel: t("断开连接", "goAuth.disconnectAction"),
    danger: true,
  });
  if (!confirmed || !current(operation)) return;
  await request("/api/go/connection", { method: "DELETE", body: "{}" }, "disconnect");
}

function render() {
  const pending = state?.pending;
  const expired = pendingExpired();
  const selected = $("#go-auth-organization")?.value;
  const host = $("#go-auth-content");
  host.replaceChildren();
  $("#go-auth-start").textContent = state?.connected
    ? t("重新登录 Go 订阅", "goAuth.reconnect")
    : t("浏览器登录 Go 订阅", "goAuth.start");
  $("#go-auth-start").disabled = busy || !!(pending && pending.status !== "failed" && !expired);
  $("#go-auth-refresh").disabled = busy;
  $("#go-auth-cancel").classList.toggle("hidden", !pending);
  $("#go-auth-cancel").disabled = busy;
  $("#go-auth-disconnect").classList.toggle("hidden", !state?.connected);
  $("#go-auth-disconnect").disabled = busy;
  if (state?.connected) {
    const account = state.account || {};
    host.append(
      el("p", {
        class: account.needs_login ? "warn" : "ok",
        text: account.needs_login
          ? t("Go 订阅登录已失效，请重新登录。手动 Go Key 不会作为备用通道。", "goAuth.needsLogin")
          : t("Go 订阅已连接，登录凭证仅保存在网关服务端。", "goAuth.connected"),
      }),
    );
    if (account.error) host.append(el("p", { class: "warn", text: account.error }));
    if (account.email) host.append(el("p", { class: "mono", text: account.email }));
    if (account.organization)
      host.append(el("p", { text: t("组织：{0}", "goAuth.organizationValue", account.organization) }));
    if (account.models != null)
      host.append(el("p", { text: t("已获取 {0} 个订阅模型。", "goAuth.models", account.models) }));
    if (account.expires_at && !account.needs_login)
      host.append(
        el("p", {
          class: "section-caption",
          text: t("访问凭证到期：{0}；网关按需自动刷新。", "goAuth.expires", localTime(account.expires_at)),
        }),
      );
  } else if (!pending) {
    host.append(el("p", {
      class: "section-caption",
      text: t("尚未连接 Go 订阅。手动配置的 Go Key 不受此登录影响。", "goAuth.disconnected"),
    }));
  }
  if (pending) {
    if (expired) {
      host.append(el("p", { class: "warn", text: t("本次登录已过期，请重新开始。", "goAuth.expired") }));
    } else if (pending.status === "failed") {
      host.append(el("p", {
        class: "warn",
        text: pending.error || t("订阅登录未完成，请重新开始。", "goAuth.failed"),
      }));
    } else if (pending.status === "choosing_org") {
      const select = el("select", { id: "go-auth-organization" });
      select.append(el("option", { value: "", text: t("请选择订阅所在组织", "goAuth.chooseOrganization") }));
      for (const organization of pending.organizations || [])
        select.append(el("option", { value: organization.id, text: organization.name || organization.id }));
      if ([...select.options].some((option) => option.value === selected)) select.value = selected;
      const confirm = el("button", {
        type: "button",
        class: "btn btn-sm btn-primary",
        text: t("连接所选组织", "goAuth.confirmOrganization"),
        disabled: busy || !select.value,
      });
      select.disabled = busy;
      select.addEventListener("change", () => (confirm.disabled = !select.value));
      confirm.addEventListener("click", () => poll(select.value));
      host.append(el("div", { class: "field" }, [
        el("label", { for: "go-auth-organization", text: t("订阅所在组织", "goAuth.organization") }),
        select,
      ]), confirm);
    } else if (pending.status === "starting") {
      host.append(el("p", { text: t("正在申请官方登录验证码…", "goAuth.starting") }));
    } else {
      host.append(el("p", { text: t("打开官方页面完成登录，并确认这是您发起的授权。", "goAuth.instructions") }));
      if (pending.user_code)
        host.append(el("p", {}, [
          el("span", { text: t("验证码：", "goAuth.userCode") }),
          el("code", { class: "mono", text: pending.user_code }),
        ]));
      const url = pending.verification_url && verificationURL(pending.verification_url);
      if (url)
        host.append(el("a", {
          class: "btn btn-sm btn-primary",
          href: url,
          target: "_blank",
          rel: "noopener noreferrer",
          text: t("打开 OpenCode 官方登录页", "goAuth.openVerification"),
        }));
      else
        host.append(el("p", { class: "warn", text: t("官方验证链接无效，请取消后重试。", "goAuth.invalidURL") }));
      if (pending.expires_at)
        host.append(el("p", {
          class: "section-caption",
          text: t("验证码有效至 {0}；授权后将自动检查结果。", "goAuth.pendingExpires", localTime(pending.expires_at)),
        }));
    }
  }
  if (failure)
    host.append(el("p", { class: "warn", role: "alert", text: t("操作失败：{0}", "goAuth.error", failure) }));
}

export function init(onConnectionChanged) {
  connectionChanged = onConnectionChanged;
  $("#go-auth-start").addEventListener("click", () =>
    request("/api/go/auth/start", { method: "POST", body: "{}" }, "start"),
  );
  $("#go-auth-refresh").addEventListener("click", refresh);
  $("#go-auth-cancel").addEventListener("click", () =>
    request("/api/go/auth", { method: "DELETE", body: "{}" }, "cancel"),
  );
  $("#go-auth-disconnect").addEventListener("click", disconnect);
  document.addEventListener("visibilitychange", () => {
    if (document.hidden) stopPolling();
    else if (active) refresh();
  });
  onChange(render);
  render();
}

export function refresh() {
  return request("/api/go/auth");
}

export function show() {
  active = true;
  if (!document.hidden) refresh();
}

export function hide() {
  active = false;
  stopPolling();
}

export function reset() {
  generation++;
  active = false;
  stopPolling();
  controller?.abort();
  controller = null;
  state = null;
  busy = false;
  failure = "";
  synchronizationPending = false;
  render();
}
