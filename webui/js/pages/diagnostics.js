// Page: 诊断 — request testing, routing, ledgers and the live event stream.
import { api } from "../api.js";
import { loadDebugModels } from "../data.js";
import * as logstream from "../logstream.js";
import * as store from "../store.js";
import { t, dateLocale, onChange } from "../i18n.js";
import {
  $,
  bindTabs,
  copyText,
  el,
  emptyRow,
  formatNumber,
  renderFacts,
  timeOnly,
  toast,
} from "../ui.js";

const keyTestLabels = () => ({
  usable: t("Key 可用", "play.key.usable"),
  rejected: t("Key 被拒绝", "play.key.rejected"),
  rate_limited: t("Key 已被接受但受到限流", "play.key.rateLimited"),
  upstream_error: t("上游异常，无法判定 Key", "play.key.upstreamError"),
  transport_error: t("网络或代理异常，无法判定 Key", "play.key.transportError"),
  unavailable: t("Key 或运行时不可用", "play.key.unavailable"),
  request_error: t("请求未成功", "play.key.requestError"),
});

function playgroundTemplate() {
  const protocol = $("#play-protocol").value;
  const model = $("#play-model").value || "YOUR_MODEL";
  const prompt = t("用一句话说明这个模型的能力。", "playground.samplePrompt");
  if (protocol === "responses") return { model, input: prompt };
  if (protocol === "anthropic")
    return {
      model,
      max_tokens: 512,
      messages: [{ role: "user", content: prompt }],
    };
  return { model, messages: [{ role: "user", content: prompt }] };
}

function resetTemplate() {
  $("#play-request").value = JSON.stringify(playgroundTemplate(), null, 2);
}

function renderKeys() {
  const select = $("#play-key");
  const selected = select.value;
  select.replaceChildren();
  select.append(el("option", { value: "auto", text: t("自动路由（匿名 / Tier 回退 / Key 轮询）", "playground.key.auto") }));
  const keys = store.get("debug")?.keys || {};
  for (const [tier, label] of [
    ["zen", "Zen Keys"],
    ["go", "Go Keys"],
  ]) {
    const items = keys[tier] || [];
    if (!items.length) continue;
    const group = el("optgroup", { label });
    for (const item of items) {
      const cooling = item.cooldown_until
        ? t(
            ` · 冷却至 ${new Date(item.cooldown_until).toLocaleTimeString(dateLocale(), { hour12: false })}`,
            "playground.key.cooling",
            new Date(item.cooldown_until).toLocaleTimeString(dateLocale(), { hour12: false }),
          )
        : "";
      group.append(
        el("option", {
          value: `${tier}:${item.id}`,
          text: `${tier.toUpperCase()} · ${item.display} · ${item.id}${cooling}`,
        }),
      );
    }
    select.append(group);
  }
  if (selected && [...select.options].some((option) => option.value === selected)) select.value = selected;
  else select.value = "auto";
}

function renderModels() {
  const select = $("#play-model");
  const selected = select.value;
  select.replaceChildren();
  (store.get("debug")?.models || []).forEach((item) => {
    select.append(el("option", { value: item.model, text: item.model }));
  });
  if (selected && [...select.options].some((option) => option.value === selected)) select.value = selected;
  if (!$("#play-request").value.trim()) resetTemplate();
  renderKeys();
}

function showResult(result) {
  const keyTest = result.key_test ? keyTestLabels()[result.key_test] || result.key_test : "";
  const state = $("#play-state");
  state.textContent = result.ok
    ? t(`请求成功 · HTTP ${result.http_status}`, "play.state.ok", result.http_status)
    : t(`请求失败 · HTTP ${result.http_status}`, "play.state.failed", result.http_status);
  state.className = `section-caption ${result.ok ? "ok" : "bad"}`;
  const route = result.route || {};
  const eligibility = route.anonymous_eligibility || {};
  const selected = result.selected_key;
  const routeLabel = route.tier
    ? `${route.tier} / ${route.anonymous ? "anonymous" : route.key_id || "key"}`
    : t("未路由", "play.route.none");
  renderFacts($("#play-facts"), [
    [t("HTTP 状态", "play.httpStatus"), result.http_status, result.ok ? "ok" : "bad"],
    [t("耗时", "common.duration"), `${result.duration_ms} ms`],
    ["Request ID", result.request_id || "—"],
    [
      t("测试模式", "play.mode"),
      selected
        ? t(`指定 ${selected.tier.toUpperCase()} Key`, "play.mode.selected", selected.tier.toUpperCase())
        : t("自动路由", "play.mode.auto"),
    ],
    [t("测试 Key", "play.testedKey"), selected ? `${selected.display} · ${selected.id}` : "—"],
    [
      t("Key 判断", "play.keyVerdict"),
      keyTest || t("自动路由未单独判定", "play.keyVerdict.none"),
      result.key_test === "usable" ? "ok" : result.key_test ? "bad" : "",
    ],
    [t("实际路由", "play.actualRoute"), routeLabel],
    [t("原生协议", "play.nativeProtocol"), route.native_protocol || "—"],
    [
      t("匿名判断", "play.anonymousVerdict"),
      t(
        `${eligibility.allowed ? "允许" : "拒绝"} · ${eligibility.source || "—"}`,
        eligibility.allowed ? "play.eligibility.allowed" : "play.eligibility.denied",
        eligibility.source || "—",
      ),
      eligibility.allowed ? "ok" : "warn",
    ],
  ]);
  $("#play-output").textContent = JSON.stringify(result.response, null, 2);
}

async function run(event) {
  event.preventDefault();
  let request;
  try {
    request = JSON.parse($("#play-request").value);
  } catch (error) {
    toast(t(`请求 JSON 无效：${error.message}`, "toast.play.invalidJson", error.message), true);
    return;
  }
  const keyValue = $("#play-key").value;
  const key =
    keyValue === "auto"
      ? { mode: "auto" }
      : { mode: "selected", tier: keyValue.split(":")[0], id: keyValue.split(":").slice(1).join(":") };
  $("#play-run").disabled = true;
  $("#play-state").textContent =
    key.mode === "selected"
      ? t("指定 Key 测试进行中…", "play.state.testingKey")
      : t("Gateway 请求进行中…", "play.state.running");
  try {
    store.set(
      "lastPlayground",
      await api("/api/debug/inference", {
        method: "POST",
        body: JSON.stringify({ protocol: $("#play-protocol").value, key, request }),
      }),
    );
  } catch (error) {
    toast(error.message, true);
    $("#play-state").textContent = t("诊断入口请求失败", "play.state.failedToCall");
  } finally {
    $("#play-run").disabled = false;
  }
}

function renderRequests() {
  const host = $("#upstream-requests");
  host.replaceChildren();
  (store.get("monitor")?.upstream?.requests || [])
    .slice(-100)
    .reverse()
    .forEach((request) => {
      const terminal = {
        stream_error: t("流式响应失败", "diag.streamError"),
        client_canceled: t("客户端已取消", "diag.clientCanceled"),
      };
      const result = terminal[request.outcome]
        ? `HTTP ${request.status} · ${terminal[request.outcome]}`
        : request.status || request.outcome;
      host.append(
        el("tr", {}, [
          el("td", { class: "mono", text: timeOnly(request.time) }),
          el("td", { class: "mono", text: request.request_id }),
          el("td", { text: request.model }),
          el("td", { text: request.tier || "—" }),
          el("td", { text: request.channel }),
          el("td", { text: request.key_id || "—" }),
          el("td", { class: "num", text: request.attempts }),
          el("td", { class: request.success ? "ok" : "bad", text: result }),
          el("td", { class: "num", text: `${request.duration_ms} ms` }),
        ]),
      );
    });
  if (!host.children.length) emptyRow(host, 9, t("尚无请求路由", "diag.noRequests"));
}

function renderAttempts() {
  const host = $("#upstream-attempts");
  host.replaceChildren();
  (store.get("monitor")?.upstream?.recent || [])
    .slice(-100)
    .reverse()
    .forEach((attempt) => {
      host.append(
        el("tr", {}, [
          el("td", { class: "mono", text: timeOnly(attempt.time) }),
          el("td", { class: "mono", text: attempt.request_id }),
          el("td", { text: attempt.model }),
          el("td", { text: `${attempt.tier} / ${attempt.channel}` }),
          el("td", { text: attempt.key_id }),
          el("td", { text: attempt.proxy_node }),
          el("td", { class: attempt.success ? "ok" : "bad", text: attempt.status || attempt.outcome }),
          el("td", { class: "num", text: `${attempt.duration_ms} ms` }),
        ]),
      );
    });
  if (!host.children.length) emptyRow(host, 8, t("尚无上游尝试", "diag.noAttempts"));
}

function renderMetadata() {
  const monitor = store.get("monitor");
  const debug = store.get("debug");
  const metadata = debug?.metadata || monitor?.resources?.metadata || {};
  const stamp = (value) => new Date(value).toLocaleString(dateLocale(), { hour12: false });
  renderFacts($("#metadata-facts"), [
    [
      t("就绪", "diag.ready"),
      metadata.ready ? t("是", "common.yes") : t("否", "common.no"),
      metadata.ready ? "ok" : "warn",
    ],
    [t("模型数", "diag.modelCount"), formatNumber(metadata.models)],
    [
      t("更新时间", "diag.updatedAt"),
      metadata.updated_at ? stamp(metadata.updated_at) : t("尚未更新", "diag.notUpdated"),
    ],
    [t("是否过期", "diag.stale"), metadata.stale ? t("是", "common.yes") : t("否", "common.no"), metadata.stale ? "warn" : "ok"],
    [t("下次刷新", "diag.nextRefresh"), metadata.next_refresh ? stamp(metadata.next_refresh) : "—"],
    [
      t("最近错误", "diag.lastError"),
      metadata.last_error || t("无", "common.none"),
      metadata.last_error ? "bad" : "ok",
    ],
  ]);
}

function renderRouteModels() {
  const debug = store.get("debug");
  const host = $("#diagnostic-models");
  if (!host || !debug) return;
  const query = $("#diagnostic-model-search").value.trim().toLowerCase();
  host.replaceChildren();
  (debug.models || [])
    .filter((item) => !query || item.model.toLowerCase().includes(query))
    .forEach((item) => {
      const eligibility = item.anonymous_eligibility || {};
      const plan = [...(item.anonymous ? ["anonymous"] : []), ...(item.key_tiers || [])];
      host.append(
        el("tr", {}, [
          el("td", { class: "mono", text: item.model }),
          el("td", { text: `${item.native_protocol} · ${item.protocol_source}` }),
          el("td", {
            class: item.route_error ? "bad" : "",
            text: item.route_error || (plan.length ? plan.join(" → ") : "—"),
          }),
          el("td", {
            class: eligibility.allowed ? "ok" : "warn",
            text: eligibility.allowed ? t("允许", "common.allowed") : t("拒绝", "common.denied"),
          }),
          el("td", { text: eligibility.source || "—" }),
          el("td", {
            class: "num",
            text: `${eligibility.input_cost == null ? "?" : eligibility.input_cost} / ${
              eligibility.output_cost == null ? "?" : eligibility.output_cost
            }`,
          }),
        ]),
      );
    });
  if (!host.children.length) emptyRow(host, 6, t("没有匹配模型", "diag.noMatch"));
}

let activeTab = "playground";

function renderLedger() {
  renderRequests();
  renderAttempts();
}

function renderRoute() {
  renderMetadata();
  renderRouteModels();
}

export default {
  id: "diagnostics",
  title: "诊断",
  sub: "请求测试、路由资格与运行流水。",
  init() {
    bindTabs($("#page-diagnostics"), (id) => {
      activeTab = id;
      logstream.setVisible(id === "logs");
      if (id === "playground") {
        if (!store.get("debug")) loadDebugModels();
        renderModels();
      }
      if (id === "ledger") renderLedger();
      if (id === "route") renderRoute();
    });
    logstream.bind();
    $("#play-protocol").addEventListener("change", resetTemplate);
    $("#play-model").addEventListener("change", () => {
      try {
        const request = JSON.parse($("#play-request").value);
        request.model = $("#play-model").value;
        $("#play-request").value = JSON.stringify(request, null, 2);
      } catch {
        resetTemplate();
      }
    });
    $("#play-reset").addEventListener("click", resetTemplate);
    $("#play-form").addEventListener("submit", run);
    $("#play-copy").addEventListener("click", () =>
      copyText($("#play-output").textContent, t("原始响应已复制", "toast.play.copied")),
    );
    $("#diagnostic-model-search").addEventListener("input", renderRouteModels);
    store.on("debug", () => {
      renderModels();
      renderRoute();
    });
    store.on("monitor", () => {
      renderLedger();
      renderMetadata();
    });
    store.on("lastPlayground", (value) => {
      if (value) showResult(value);
    });
    // The module owns every string it renders, so a language switch rebuilds it.
    onChange(() => {
      renderModels();
      renderLedger();
      renderMetadata();
      renderRoute();
      const last = store.get("lastPlayground");
      if (last) showResult(last);
    });
  },
  show() {
    if (!store.get("debug")) loadDebugModels();
    renderModels();
    renderLedger();
    renderRoute();
    if (activeTab === "logs") logstream.setVisible(true);
  },
  hide() {
    logstream.setVisible(false);
  },
  refresh() {
    if (activeTab === "logs") logstream.render();
  },
};
