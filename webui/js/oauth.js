import { api, hasSession } from "./api.js";
import * as store from "./store.js";
import { t, onChange } from "./i18n.js";
import { $, confirmDialog, el, localTime } from "./ui.js";

let connectionChanged = null;
let synchronizationVersion = 0;

const plans = [
  {
    id: "go",
    copy: {
      start: "浏览器登录 Go OAuth",
      reconnect: "重新登录 Go OAuth",
      disconnect: "断开 Go OAuth 登录",
      disconnectConfirm: "仅移除网关保存的 Go OAuth 登录，不删除手动 Go Key，也不会撤销 OpenCode 官方会话。是否继续？",
      connected: "Go OAuth 已连接，登录凭证仅保存在网关服务端。",
      needsLogin: "Go OAuth 登录已失效，请重新登录。手动 Go Key 不会作为备用通道。",
      disconnected: "尚未连接 Go OAuth。手动配置的 Go Key 不受此登录影响。",
      models: "已获取 {0} 个 Go 订阅模型。",
      failed: "Go OAuth 登录未完成，请重新开始。",
      chooseOrganization: "请选择 Go OAuth 授权组织",
      organization: "Go OAuth 授权组织",
    },
  },
  {
    id: "zen",
    copy: {
      start: "浏览器登录 Zen / Console PAYG",
      reconnect: "重新登录 Zen / Console PAYG",
      disconnect: "断开 Zen / Console PAYG 登录",
      disconnectConfirm: "仅移除网关保存的 Zen PAYG 登录，保留手动 Zen Key、Go Key 和 Go OAuth 连接，不撤销 OpenCode 官方会话。是否继续？",
      connected: "Zen / Console PAYG 已连接；经此通道的请求按 credits 计费，登录凭证仅保存在服务端。",
      needsLogin: "Zen / Console PAYG 登录已失效，请重新登录。手动 Key 与 Go OAuth 连接仍保留。",
      disconnected: "尚未连接 Zen / Console PAYG。此登录与 Go OAuth 及手动 Key 分开管理。",
      models: "已获取 {0} 个 Zen PAYG 模型；模型数量不代表剩余 credits。",
      failed: "Zen PAYG 登录未完成，请重新开始。",
      chooseOrganization: "请选择承担 PAYG 费用的组织",
      organization: "PAYG 计费组织",
    },
  },
].map(createOAuthPanel);

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

function createOAuthPanel(plan) {
  let state = null;
  let timer = null;
  let active = false;
  let busy = false;
  let confirming = false;
  let generation = 0;
  let controller = null;
  let failure = "";
  let synchronizationPending = false;
  const node = (name) => $(`#${plan.id}-oauth-${name}`);
  const message = (name, ...values) => t(plan.copy[name], `${plan.id}OAuth.${name}`, ...values);
  const oauthPath = `/api/${plan.id}/auth`;

  function stopPolling() {
    clearTimeout(timer);
    timer = null;
  }

  function pendingExpired() {
    const expires = Date.parse(state?.pending?.expires_at);
    return Number.isFinite(expires) && expires <= Date.now();
  }

  function current(operation) {
    return operation === generation && hasSession();
  }

  async function synchronize(operation, signal) {
    const version = ++synchronizationVersion;
    const [config, debug] = await Promise.all([
      api("/api/config", { signal }), api("/api/debug/models", { signal }),
    ]);
    if (!current(operation) || version !== synchronizationVersion) return;
    store.set("config", config);
    if (debug.last_inference) store.set("lastPlayground", debug.last_inference);
    store.set("debug", debug);
    await connectionChanged?.();
    if (current(operation) && version === synchronizationVersion) synchronizationPending = false;
  }

  function schedulePoll() {
    stopPolling();
    const pending = state?.pending;
    if (
      !active ||
      document.hidden ||
      busy ||
      confirming ||
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
    if (busy || confirming || !hasSession()) return;
    stopPolling();
    const operation = generation;
    const previous = connectionID(state);
    busy = true;
    failure = "";
    controller = new AbortController();
    const signal = controller.signal;
    render();
    try {
      const next = await api(path, { ...options, signal });
      if (!current(operation)) return;
      state = next;
      render();
      if (
        action === "disconnect" ||
        previous !== connectionID(next) ||
        (action === "poll" && next.connected && !next.pending)
      )
        synchronizationPending = true;
      if (synchronizationPending) await synchronize(operation, signal);
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
    return request(`${oauthPath}/poll`, { method: "POST", body: JSON.stringify(body) }, "poll");
  }

  async function confirm(options) {
    if (busy || confirming || !hasSession()) return false;
    const operation = generation;
    confirming = true;
    stopPolling();
    render();
    try {
      return (await confirmDialog(options)) && current(operation);
    } finally {
      if (operation === generation) {
        confirming = false;
        render();
        schedulePoll();
      }
    }
  }

  async function start() {
    const operation = generation;
    if (plan.id === "zen") {
      const accepted = await confirm({
        title: t("确认启用 Zen PAYG 计费通道", "zenOAuth.confirmTitle"),
        message: t(
          "启用后走 Zen OAuth 的请求按 PAYG 消耗 credits，不使用 Go 订阅额度；不会自动将 Go 失败转入本通道，模型仍依配置 prefer 选路。是否确认并继续官方授权？",
          "zenOAuth.confirmPayg",
        ),
        confirmLabel: t("确认 PAYG 计费并继续", "zenOAuth.confirmAction"),
      });
      if (!accepted || !current(operation)) return;
    }
    return request(`${oauthPath}/start`, {
      method: "POST", body: JSON.stringify(plan.id === "zen" ? { acknowledge_payg: true } : {}),
    }, "start");
  }

  async function disconnect() {
    const operation = generation;
    const accepted = await confirm({
      title: message("disconnect"), message: message("disconnectConfirm"),
      confirmLabel: t("断开连接", "oauth.disconnectAction"), danger: true,
    });
    if (!accepted || !current(operation)) return;
    return request(`/api/${plan.id}/connection`, { method: "DELETE", body: "{}" }, "disconnect");
  }

  function render() {
    const pending = state?.pending;
    const expired = pendingExpired();
    const selected = node("organization")?.value;
    const host = node("content");
    const working = busy || confirming;
    host.replaceChildren();
    node("start").textContent = message(state?.connected ? "reconnect" : "start");
    node("start").disabled = working || !!(pending && pending.status !== "failed" && !expired);
    node("refresh").disabled = working;
    node("cancel").classList.toggle("hidden", !pending);
    node("cancel").disabled = working;
    node("disconnect").classList.toggle("hidden", !state?.connected);
    node("disconnect").disabled = working;
    if (state?.connected) {
      const account = state.account || {};
      host.append(el("p", {
        class: account.needs_login ? "warn" : "ok",
        text: message(account.needs_login ? "needsLogin" : "connected"),
      }));
      if (account.error) host.append(el("p", { class: "warn", text: account.error }));
      if (account.email) host.append(el("p", { class: "mono", text: account.email }));
      if (account.organization)
        host.append(el("p", { text: t("组织：{0}", "oauth.organizationValue", account.organization) }));
      if (account.models != null) host.append(el("p", { text: message("models", account.models) }));
      if (account.expires_at && !account.needs_login)
        host.append(el("p", {
          class: "section-caption",
          text: t("访问凭证到期：{0}；网关按需自动刷新。", "oauth.expires", localTime(account.expires_at)),
        }));
    } else if (!pending) {
      host.append(el("p", { class: "section-caption", text: message("disconnected") }));
    }
    if (pending) {
      if (expired) {
        host.append(el("p", { class: "warn", text: t("本次登录已过期，请重新开始。", "oauth.expired") }));
      } else if (pending.status === "failed") {
        host.append(el("p", { class: "warn", text: pending.error || message("failed") }));
      } else if (pending.status === "choosing_org") {
        const select = el("select", { id: `${plan.id}-oauth-organization` });
        select.append(el("option", { value: "", text: message("chooseOrganization") }));
        for (const organization of pending.organizations || [])
          select.append(el("option", { value: organization.id, text: organization.name || organization.id }));
        if ([...select.options].some((option) => option.value === selected)) select.value = selected;
        const choose = el("button", {
          type: "button", class: "btn btn-sm btn-primary",
          text: t("连接所选组织", "oauth.confirmOrganization"), disabled: working || !select.value,
        });
        select.disabled = working;
        select.addEventListener("change", () => (choose.disabled = working || !select.value));
        choose.addEventListener("click", () => poll(select.value));
        host.append(el("div", { class: "field" }, [
          el("label", { for: `${plan.id}-oauth-organization`, text: message("organization") }), select,
        ]), choose);
      } else if (pending.status === "starting") {
        host.append(el("p", { text: t("正在申请官方登录验证码…", "oauth.starting") }));
      } else {
        host.append(el("p", { text: t("打开官方页面完成登录，并确认这是您发起的授权。", "oauth.instructions") }));
        if (pending.user_code)
          host.append(el("p", {}, [
            el("span", { text: t("验证码：", "oauth.userCode") }),
            el("code", { class: "mono", text: pending.user_code }),
          ]));
        const url = pending.verification_url && verificationURL(pending.verification_url);
        if (url)
          host.append(el("a", {
            class: "btn btn-sm btn-primary", href: url, target: "_blank", rel: "noopener noreferrer",
            text: t("打开 OpenCode 官方登录页", "oauth.openVerification"),
          }));
        else
          host.append(el("p", { class: "warn", text: t("官方验证链接无效，请取消后重试。", "oauth.invalidURL") }));
        if (pending.expires_at)
          host.append(el("p", {
            class: "section-caption",
            text: t("验证码有效至 {0}；授权后将自动检查结果。", "oauth.pendingExpires", localTime(pending.expires_at)),
          }));
      }
    }
    if (failure)
      host.append(el("p", { class: "warn", role: "alert", text: t("操作失败：{0}", "oauth.error", failure) }));
  }

  function refresh() {
    return request(oauthPath);
  }

  return {
    init() {
      node("start").addEventListener("click", start);
      node("refresh").addEventListener("click", refresh);
      node("cancel").addEventListener("click", () => request(oauthPath, { method: "DELETE", body: "{}" }, "cancel"));
      node("disconnect").addEventListener("click", disconnect);
      document.addEventListener("visibilitychange", () => {
        if (document.hidden) stopPolling();
        else if (active) refresh();
      });
      onChange(render);
      render();
    },
    refresh,
    show() {
      active = true;
      if (!document.hidden) return refresh();
    },
    hide() {
      active = false;
      stopPolling();
    },
    reset() {
      generation++;
      active = false;
      stopPolling();
      controller?.abort();
      controller = null;
      state = null;
      busy = false;
      confirming = false;
      failure = "";
      synchronizationPending = false;
      render();
    },
  };
}

export function init(onConnectionChanged) {
  connectionChanged = onConnectionChanged;
  for (const plan of plans) plan.init();
}

export function refresh() {
  return Promise.all(plans.map((plan) => plan.refresh()));
}

export function show() {
  return Promise.all(plans.map((plan) => plan.show()));
}

export function hide() {
  for (const plan of plans) plan.hide();
}

export function reset() {
  synchronizationVersion++;
  for (const plan of plans) plan.reset();
}
