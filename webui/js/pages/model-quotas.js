// Page: 模型与额度 — Go quota windows and free model availability.
import { api, hasSession } from "../api.js";
import { loadDebugModels } from "../data.js";
import { t, onChange } from "../i18n.js";
import { $, bindTabs, copyText, el, emptyRow, formatNumber, localTime, timeOnly, toast } from "../ui.js";

const windows = () => [
  ["rolling", t("5 小时窗口", "quotas.window.hour")],
  ["weekly", t("每周窗口", "quotas.window.week")],
  ["monthly", t("每月窗口", "quotas.window.month")],
];

let data = null;
let timer = null;
let loading = false;
let activeTab = "quota";
let freeQuotaData = null;
let freeQuotaStatus = null;
let freeQuotaLoading = false;
let freeQuotaTimer = null;
let freeQuotaProbing = false;
let freeQuotaProbeKey = null;
const freeQuotaProbeResults = new Map();

async function load() {
  if (loading) return;
  loading = true;
  $("#quota-refresh").disabled = true;
  $("#quota-refresh").textContent = t("读取中…", "quotas.loading");
  try {
    const value = await api("/api/model-quotas");
    if (!hasSession()) return;
    data = value;
    const select = $("#quota-account");
    const selected = select.value;
    select.replaceChildren();
    for (const account of value.accounts || []) {
      select.append(el("option", { value: account.id, text: `Go Key ${account.display}` }));
    }
    if ([...select.options].some((option) => option.value === selected)) select.value = selected;
    if (!select.options.length)
      select.append(el("option", { value: "", text: t("尚未配置 Go Key", "quotas.noKey") }));
    render();
  } catch (error) {
    $("#quota-status").textContent = t(
      `查询失败：${error.message}。已有显示仅为上次查询结果。`,
      "quotas.queryFailed",
      error.message,
    );
  } finally {
    loading = false;
    $("#quota-refresh").disabled = false;
    $("#quota-refresh").textContent = t("刷新额度", "quotas.refresh");
  }
}

function render() {
  if (!data) return;
  const account = (data.accounts || []).find((item) => item.id === $("#quota-account").value);
  const stale = account?.status === "stale";
  const cards = $("#quota-cards");
  cards.replaceChildren();
  $("#quota-status").textContent = !account
    ? t("请先在配置中心添加 OpenCode Go Key。", "quotas.addKeyFirst")
    : account.error
      ? stale
        ? t("{0}；下方显示上次成功数据，当前额度未知。", "quotas.staleError", account.error)
        : t("{0}；当前额度未知。", "quotas.error", account.error)
      : t("已连接 OpenCode · 共享套餐额度。多个 Key 的额度不相加。", "quotas.connected");
  $("#quota-updated").textContent = account?.fetched_at
    ? t(
        `${stale ? "上次成功查询" : "额度更新时间"}：${localTime(account.fetched_at)} · 缓存 60 秒 · 来源：OpenCode Go /v1/usage`,
        stale ? "quotas.lastSuccess" : "quotas.updated",
        localTime(account.fetched_at),
      )
    : "";
  for (const [id, label] of windows()) {
    const item = account?.windows?.[id];
    const progress = el("progress", { max: 100, "aria-label": t(`${label}剩余额度`, "quotas.window.aria", label) });
    progress.value = item ? item.remaining_percent : 0;
    cards.append(
      el("div", { class: `quota-card${item?.remaining_percent < 20 ? " quota-low" : ""}` }, [
        el("h3", { text: label }),
        el("div", {
          class: "quota-value",
          text: item ? `≈ ${item.remaining_percent}%` : t("未知", "common.unknown"),
        }),
        progress,
        el("div", {
          class: "quota-reset",
          text: item
            ? `${stale ? t("历史剩余", "quotas.remainingHistory") : t("剩余", "quotas.remaining")} · ${
                item.status === "rate-limited" ? t("已限额 · ", "quotas.rateLimited") : ""
              }${t("重置于 {0}", "quotas.resetsAt", localTime(item.resets_at))}`
            : t("等待上游额度数据", "quotas.waiting"),
        }),
      ]),
    );
  }
  const query = $("#quota-model-search").value.trim().toLowerCase();
  const allModels = data.models || [];
  const models = allModels.filter((model) => model.id.toLowerCase().includes(query));
  $("#quota-model-count").textContent = `${models.length} / ${allModels.length}`;
  const catalog = data.catalog;
  const catalogUpdated =
    catalog?.updated_at && !catalog.updated_at.startsWith("0001-")
      ? localTime(catalog.updated_at)
      : t("尚未就绪", "common.notReady");
  $("#quota-catalog-status").textContent = `${catalog?.stale ? t("模型目录暂未更新，显示缓存 · ", "quotas.catalogStale") : ""}${t(
    `目录更新时间：${catalogUpdated}`,
    "quotas.catalogUpdated",
    catalogUpdated,
  )}`;
  const host = $("#quota-models");
  host.replaceChildren();
  for (const model of models) {
    const copy = el("button", { class: "btn btn-sm btn-ghost", text: t("复制名称", "quotas.copyName") });
    copy.addEventListener("click", () => copyText(model.id));
    const row = el("tr", {}, [
      el("td", { class: "mono", text: model.id }),
      el("td", {
        text:
          { chat: "Chat Completions", responses: "Responses", anthropic: "Messages" }[model.protocol] ||
          model.protocol,
      }),
    ]);
    for (const [id] of windows()) {
      const item = account?.windows?.[id];
      row.append(
        el("td", {
          class: "num",
          text: item
            ? `≈ ${item.remaining_percent}%${stale ? t("（历史）", "quotas.history") : ""}`
            : t("未知", "common.unknown"),
        }),
      );
    }
    row.append(el("td", {}, [copy]));
    host.append(row);
  }
  if (!models.length)
    emptyRow(
      host,
      6,
      allModels.length
        ? t("没有匹配的模型", "diag.noMatch")
        : t("模型目录尚未就绪，请稍后刷新", "quotas.catalogPending"),
    );
}

const date = (value) =>
  value && !value.startsWith("0001-") ? localTime(value) : t("待探测", "availability.pending");

async function loadAvailability() {
  try {
    const value = await api("/api/models/availability");
    $("#availability-error").textContent = "";
    const host = $("#availability-models");
    host.replaceChildren();
    for (const item of value.models) {
      const restore = el("button", {
        type: "button",
        class: "btn btn-sm btn-ghost",
        text: t("恢复启用", "availability.restore"),
        disabled: !item.disabled,
      });
      const toggle = el("button", {
        type: "button",
        class: "btn btn-sm btn-ghost",
        text: item.manual
          ? t("切回自动", "availability.switchAuto")
          : t("切为手动", "availability.switchManual"),
      });
      const run = async (path, body) => {
        restore.disabled = true;
        toggle.disabled = true;
        try {
          await api(path, { method: "POST", body: JSON.stringify(body) });
          await loadAvailability();
          await loadDebugModels();
        } catch (error) {
          $("#availability-error").textContent = error.message;
          restore.disabled = !item.disabled;
          toggle.disabled = false;
        }
      };
      restore.addEventListener("click", () => run("/api/models/restore", { model: item.model }));
      toggle.addEventListener("click", () =>
        run("/api/models/manual", { model: item.model, manual: !item.manual }),
      );
      host.append(
        el("tr", {}, [
          el("td", { class: "mono", text: item.model }),
          el("td", {
            class: item.disabled ? "warn" : "ok",
            text: item.disabled ? t("已禁用", "state.disabled") : t("已启用", "state.enabled"),
          }),
          el("td", {
            text:
              [item.channel, item.reason].filter(Boolean).join(" / ") ||
              t("待探测", "availability.pending"),
          }),
          el("td", {
            text: item.manual
              ? t("手动", "availability.manual")
              : t("自动", "availability.auto"),
          }),
          el("td", {
            text: item.auto_effort?.effort
              ? `${item.auto_effort.effort}（${date(item.effort_checked_at)}）`
              : t("未验证", "availability.unverified"),
          }),
          el("td", { class: "mono", text: date(item.checked_at) }),
          el("td", {
            class: "mono",
            text: item.manual
              ? t("手动控制，不自动探测", "availability.manualControl")
              : date(item.next_check),
          }),
          el("td", {}, [el("div", { class: "key-actions" }, [restore, toggle])]),
        ]),
      );
    }
    if (!value.models.length)
      $("#availability-error").textContent = t(
        "当前模型目录中没有符合免费资格的模型。",
        "availability.noneEligible",
      );
  } catch (error) {
    $("#availability-error").textContent = error.message;
  }
}

async function refreshAvailability() {
  const button = $("#availability-refresh");
  button.disabled = true;
  try {
    await api("/api/models/probe", { method: "POST", body: "{}" });
    toast(t("已安排重新探测，结果将在探测后陆续更新。", "availability.probeScheduled"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
  await loadAvailability();
  // The sweep re-probes one model at a time; reload once more when the first
  // fresh results have landed.
  setTimeout(loadAvailability, 20000);
}

const freeRows = (value) => (Array.isArray(value?.egresses) ? value.egresses : []);
const freeNum = (value, fallback = 0) => {
  const number = Number(value);
  return Number.isFinite(number) ? number : fallback;
};
const freeState = (row) =>
  row.limit_source === "learned"
    ? "learned"
    : row.limit_source === "assumed"
      ? "assumed"
      : "unknown";
const freeCounted = (row) =>
  row.seen === true && row.remaining != null && freeState(row) !== "unknown" && freeNum(row.limit) > 0;
const freeKey = (row) => `${row.kind || "unknown"}|${row.index ?? ""}|${row.label || ""}`;
const freeProbeable = (row) =>
  Boolean(row.probe_supported) && ["proxy", "direct", "warp"].includes(row.kind);
const freeLabel = (row) =>
  row.kind === "warp"
    ? row.egress || row.label || "WARP"
    : row.label || row.egress || (row.kind === "direct" ? "direct" : "—");

function freeSetStatus(original, key, ...args) {
  freeQuotaStatus = { original, key, args };
  $("#freequota-status").textContent = key ? t(original, key, ...args) : original;
}

const freeSetText = (id, text) => {
  const node = $(`#${id}`);
  if (node) node.textContent = text;
};

function freeHealth(row) {
  if (row.blocked && row.healthy === false)
    return { tone: "bad", text: t("已阻断", "quotas.freeq.health.blocked") };
  if (row.cooling) return { tone: "warn", text: t("冷却中", "quotas.freeq.health.cooling") };
  if (row.health_known === false)
    return { tone: "freeq-health-unknown", text: t("健康未知", "quotas.freeq.health.unknown") };
  if (row.healthy) return { tone: "ok", text: t("正常", "quotas.freeq.health.ok") };
  return { tone: "bad", text: t("异常", "quotas.freeq.health.bad") };
}

function freeCountdown(value) {
  const diff = value ? new Date(value).getTime() - Date.now() : NaN;
  if (!Number.isFinite(diff)) return "—";
  const minutes = Math.floor(diff / 60000);
  if (minutes < 1) return t("不足 1 分", "quotas.freeq.countdown.soon");
  const hours = Math.floor(minutes / 60);
  return hours > 0
    ? t(`${hours} 小时 ${minutes % 60} 分`, "quotas.freeq.countdown.hm", hours, minutes % 60)
    : t(`${minutes} 分`, "quotas.freeq.countdown.m", minutes);
}

function freeStack(host, items) {
  host.replaceChildren();
  for (const item of items) {
    const value = Math.max(0, freeNum(item.value));
    if (!value) continue;
    const segment = el("div", {
      class: `freeq-seg ${item.className || ""}`.trim(),
      title: item.title || null,
    });
    segment.style.flex = `${value} 1 0`;
    host.append(segment);
  }
}

function freeHistory(row, value) {
  const byDate = new Map(
    (Array.isArray(row.history) ? row.history : [])
      .filter((entry) => entry?.date)
      .map((entry) => [String(entry.date).slice(0, 10), entry]),
  );
  const end = /^\d{4}-\d{2}-\d{2}/.test(String(value?.date || ""))
    ? String(value.date).slice(0, 10)
    : new Date().toISOString().slice(0, 10);
  const endMs = Date.parse(`${end}T00:00:00Z`);
  if (!Number.isFinite(endMs)) return null;
  const limit = freeNum(row.limit);
  const bars = [];
  for (let offset = 13; offset >= 0; offset -= 1) {
    const date = new Date(endMs - offset * 86400000).toISOString().slice(0, 10);
    const entry = byDate.get(date) || null;
    const used = freeNum(entry?.used);
    const ratio = limit > 0 ? used / limit : 0;
    const bucket =
      !entry || used <= 0
        ? 0
        : 1 + [0.25, 0.45, 0.65, 0.85].filter((threshold) => ratio >= threshold).length;
    const classes = ["freeq-hist", entry ? "" : "freeq-hist-empty"];
    if (freeNum(entry?.quota_429) > 0 || entry?.exhausted) classes.push("freeq-hist-bad");
    else if (used > 0) classes.push("freeq-hist-ok");
    const bar = el("span", {
      class: classes.filter(Boolean).join(" "),
      title: t(
        `${date} · 已用 ${used} · 429 ${freeNum(entry?.quota_429)}`,
        "quotas.freeq.history.title",
        date,
        used,
        freeNum(entry?.quota_429),
      ),
    });
    bar.style.height = bucket === 0 ? "2px" : `${bucket * 20}%`;
    bars.push(bar);
  }
  return el(
    "div",
    {
      class: "freeq-history",
      role: "img",
      "aria-label": t("最近 14 天", "quotas.freeq.history.label"),
    },
    bars,
  );
}

function freeDistribution(rows) {
  const counts = { healthy: 0, cooling: 0, blocked: 0, exhausted: 0, unobserved: 0, other: 0 };
  for (const row of rows) {
    if (row.exhausted) counts.exhausted += 1;
    else if (row.blocked) counts.blocked += 1;
    else if (row.cooling) counts.cooling += 1;
    else if (!row.seen) counts.unobserved += 1;
    else if (freeHealth(row).tone === "ok") counts.healthy += 1;
    else counts.other += 1;
  }
  return counts;
}

function freeRenderOverview(value, rows) {
  const learned = rows.filter((row) => row.seen && freeState(row) === "learned");
  const assumed = rows.filter((row) => row.seen && freeState(row) === "assumed");
  const unknown = rows.filter((row) => row.seen && freeState(row) === "unknown");
  const unobserved = rows.filter((row) => !row.seen);
  const planned = rows.filter((row) => !row.seen && row.limit > 0 && freeState(row) !== "unknown");
  const plannedTotal = planned.reduce((sum, row) => sum + freeNum(row.limit), 0);
  const counted = rows.filter(freeCounted);
  const capacity = counted.reduce((sum, row) => sum + freeNum(row.limit), 0);
  const remaining = counted.reduce((sum, row) => sum + freeNum(row.remaining), 0);
  const used = counted.reduce(
    (sum, row) => sum + Math.max(0, freeNum(row.limit) - freeNum(row.remaining)),
    0,
  );
  const known = remaining;
  const countdown = freeCountdown(value.resets_at);
  const unknownish = unknown.length + unobserved.length;
  const risk = rows.filter((row) => row.exhausted || row.cooling || row.blocked).length;

  const remainingEl = $("#freequota-kpi-remaining");
  remainingEl.textContent = counted.length
    ? formatNumber(known)
    : planned.length
      ? `≈ ${formatNumber(plannedTotal)}`
      : t("未知", "common.unknown");
  remainingEl.title =
    !counted.length && planned.length
      ? t(
          `仅有假定：${planned.length} 个出口的假定上限，非实测。`,
          "quotas.freeq.kpi.assumedOnlyTitle",
          planned.length,
        )
      : t(
          `估算：合计 ${learned.length} 个已学习、${assumed.length} 个假定出口的剩余；未知与未观察不计入，也非上游权威。`,
          "quotas.freeq.kpi.estimateTitle",
          learned.length,
          assumed.length,
        );
  freeSetText("freequota-kpi-used", formatNumber(value.total_used ?? 0));
  freeSetText("freequota-kpi-reset", countdown);
  freeSetText("freequota-kpi-reset-note", localTime(value.resets_at));

  let verdict = t("尚无可用上限估算", "quotas.freeq.verdict.none");
  if (learned.length && assumed.length)
    verdict = t(
      `已学习 ${learned.length}/${rows.length} 个出口，另有 ${assumed.length} 个假定`,
      "quotas.freeq.verdict.mixed",
      learned.length,
      rows.length,
      assumed.length,
    );
  else if (learned.length)
    verdict = t(
      `已学习 ${learned.length}/${rows.length} 个出口的上限`,
      "quotas.freeq.verdict.learned",
      learned.length,
      rows.length,
    );
  else if (assumed.length)
    verdict = t(
      `基于 ${assumed.length} 个出口的起始假定，尚未学习到真实上限`,
      "quotas.freeq.verdict.assumed",
      assumed.length,
    );
  else if (planned.length)
    verdict = t(
      `基于 ${planned.length} 个出口的起始假定，尚未学习到真实上限`,
      "quotas.freeq.verdict.assumed",
      planned.length,
    );
  const pieces = [verdict];
  if (unknownish)
    pieces.push(
      t(`${unknownish} 个出口未知或未观察（未计入）`, "quotas.freeq.kpi.unknownish", unknownish),
    );
  pieces.push(t(`距重置约 ${countdown}`, "quotas.freeq.verdict.reset", countdown));
  if (risk)
    pieces.push(
      t(`风险出口 ${risk} 个（耗尽/冷却/阻断）。`, "quotas.freeq.verdict.risk", risk),
    );
  $("#freequota-verdict").textContent = pieces.join(" · ");

  const usageTrack = $("#freequota-usage-track");
  const usageValue = $("#freequota-usage-value");
  if (capacity > 0) {
    const group = (state) => counted.filter((row) => freeState(row) === state);
    const sum = (list, key) => list.reduce((total, row) => total + freeNum(row[key]), 0);
    const lRows = group("learned");
    const aRows = group("assumed");
    const lCap = sum(lRows, "limit");
    const aCap = sum(aRows, "limit");
    const lLeft = sum(lRows, "remaining");
    const aLeft = sum(aRows, "remaining");
    const lUsed = Math.max(0, lCap - lLeft);
    const aUsed = Math.max(0, aCap - aLeft);
    const learnedLabel = t("已学习", "quotas.freeq.source.learned");
    const assumedLabel = t("假定", "quotas.freeq.source.assumed");
    freeStack(usageTrack, [
      {
        value: lUsed,
        className: "freeq-seg-learned-used",
        title: `${learnedLabel} ${formatNumber(lUsed)} / ${formatNumber(lCap)}`,
      },
      {
        value: lLeft,
        className: "freeq-seg-learned-rest",
        title: `${learnedLabel} ${formatNumber(lLeft)}`,
      },
      {
        value: aUsed,
        className: "freeq-seg-assumed-used",
        title: `${assumedLabel} ${formatNumber(aUsed)} / ${formatNumber(aCap)}`,
      },
      {
        value: aLeft,
        className: "freeq-seg-assumed-rest",
        title: `${assumedLabel} ${formatNumber(aLeft)}`,
      },
    ]);
    const percent = Math.round((used / capacity) * 1000) / 10;
    usageValue.textContent = `${formatNumber(used)} / ${formatNumber(capacity)} · ${percent}%`;
    usageValue.title = t(
      `已用 ${formatNumber(used)} / 容量 ${formatNumber(capacity)} · ${percent}%（已学习 ${formatNumber(lCap)} · 假定 ${formatNumber(aCap)}）`,
      "quotas.freeq.capacity.valueTitle",
      formatNumber(used),
      formatNumber(capacity),
      percent,
      formatNumber(lCap),
      formatNumber(aCap),
    );
    usageTrack.setAttribute("aria-label", usageValue.title);
  } else {
    freeStack(usageTrack, []);
    usageValue.textContent = "—";
    usageValue.removeAttribute("title");
    usageTrack.removeAttribute("aria-label");
  }

  const bucketRow = (prefix, count, className, note) => {
    const id = `freequota-${prefix}`;
    $(`#${id}-row`).classList.toggle("hidden", count === 0);
    if (!count) return;
    freeStack($(`#${id}-track`), [
      { value: count, className, title: note },
      { value: Math.max(0, rows.length - count) },
    ]);
    freeSetText(`${id}-value`, formatNumber(count));
  };
  bucketRow("unknown", unknown.length, "freeq-seg-unknown", t(`${unknown.length} 个出口的上限未知，未计入容量`, "quotas.freeq.capacity.unknownNote", unknown.length));
  bucketRow("unobserved", unobserved.length, "freeq-seg-unobserved", t(`${unobserved.length} 个出口尚未观察；假定合计 ${formatNumber(plannedTotal)}，未计入容量`, "quotas.freeq.capacity.unobservedNote", unobserved.length, formatNumber(plannedTotal)));

  const counts = freeDistribution(rows);
  const items = [
    ["healthy", t("正常", "quotas.freeq.dist.healthy")],
    ["cooling", t("冷却", "quotas.freeq.dist.cooling")],
    ["blocked", t("阻断", "quotas.freeq.dist.blocked")],
    ["exhausted", t("耗尽", "quotas.freeq.dist.exhausted")],
    ["unobserved", t("未观察", "quotas.freeq.dist.unobserved")],
    ["other", t("其他", "quotas.freeq.dist.other")],
  ];
  const distribution = $("#freequota-distribution-track");
  freeStack(distribution, items.map(([key, label]) => ({ value: counts[key], className: `freeq-seg-${key}`, title: `${label}: ${formatNumber(counts[key])}` })));
  distribution.setAttribute("aria-label", `${t("出口状态", "quotas.freeq.dist.title")}: ${items.map(([key, label]) => `${label} ${formatNumber(counts[key])}`).join(" · ")}`);
  const legend = $("#freequota-distribution-legend");
  legend.replaceChildren();
  for (const [key, label] of items)
    if (counts[key])
      legend.append(
        el("span", { class: "freeq-legend-item" }, [
          el("span", { class: `freeq-dot freeq-seg-${key}` }),
          el("span", { text: label }),
          el("span", { text: formatNumber(counts[key]) }),
        ]),
      );
}

const FREE_PROBE_TEXT = {
  quota: ["429 额度耗尽", "quotas.freeq.probeQuota", "warn"],
  session_risk: ["429 会话风险", "quotas.freeq.probeSession", "warn"],
  rate_limited: ["429 限流", "quotas.freeq.probeRate", "warn"],
  blocked: ["403 阻断", "quotas.freeq.probeBlocked", "bad"],
  auth: ["认证失败", "quotas.freeq.probeAuth", "bad"],
  transport_error: ["超时/传输错误", "quotas.freeq.probeTransport", "bad"],
};

function freeProbeLabel(probe) {
  const latency = freeNum(probe.latency_ms) > 0 ? ` · ${probe.latency_ms}ms` : "";
  const detail = `${probe.status || "—"}${latency}`;
  if (probe.classification === "ok")
    return { tone: "ok", text: t(detail, "quotas.freeq.probeOk", detail) };
  const entry = FREE_PROBE_TEXT[probe.classification];
  if (entry) return { tone: entry[2], text: t(entry[0], entry[1]) };
  const error = probe.error ? ` · ${probe.error}` : "";
  return { tone: "bad", text: `${t("探测失败", "quotas.freeq.probeError")}${error}` };
}

function freeRecordProbes(probes) {
  for (const probe of probes || []) freeQuotaProbeResults.set(freeKey(probe), probe);
}

function freeRow(row, value) {
  const health = freeHealth(row);
  const state = freeState(row);
  const label = freeLabel(row);
  const limit = freeNum(row.limit);
  const used = freeNum(row.used);
  const remainingKnown = row.seen && state !== "unknown" && row.remaining != null;
  const healthTitle = [health.text, row.pool ? t("池成员", "quotas.freeq.health.pool") : ""].filter(Boolean).join(" · ");
  const egressCell = el("td", { title: healthTitle }, [
    el("div", { class: "freeq-egress" }, [
      el("span", { class: `freeq-health-dot ${health.tone}`.trim(), title: healthTitle, "aria-label": healthTitle }),
      el("span", { class: "mono", text: label }),
    ]),
  ]);
  const stateLabel = state === "learned" ? t("已学习", "quotas.freeq.source.learned") : state === "assumed" ? t("假定", "quotas.freeq.source.assumed") : t("未知", "quotas.freeq.source.unknown");
  const assumedLeft = !row.seen && state !== "unknown" && limit > 0;
  const usedText = row.seen ? formatNumber(used) : "—";
  const numbers = assumedLeft
    ? `— / ${formatNumber(limit)}`
    : row.seen && limit > 0
      ? `${usedText} / ${formatNumber(limit)}`
      : usedText;
  const left = remainingKnown
    ? t(`剩余 ${formatNumber(row.remaining)}`, "quotas.freeq.today.left", formatNumber(row.remaining))
    : assumedLeft
      ? t(`≈ ${formatNumber(limit)}（假定，未验证）`, "quotas.freeq.today.assumedLeft", formatNumber(limit))
      : "—";
  const line = el("div", { class: "freeq-today-line" }, [
    el("span", { class: `freeq-src freeq-src-${state}`, text: stateLabel }),
    el("span", { class: "freeq-used", text: numbers }),
    el("span", { class: remainingKnown ? "freeq-left" : "freeq-left freeq-left-none", text: left }),
  ]);
  const bars = [];
  if (row.seen && state !== "unknown" && limit > 0)
    bars.push(
      el("progress", {
        class: `freeq-progress${state === "assumed" ? " freeq-progress-assumed" : ""}`,
        max: limit,
        value: Math.min(used, limit),
        "aria-label": `${stateLabel} ${formatNumber(used)} / ${formatNumber(limit)}`,
      }),
    );
  if (state === "learned") {
    const history = freeHistory(row, value);
    if (history) bars.push(history);
  }
  const todayCell = el("td", { class: "freeq-today", title: state === "assumed" ? t("先验假定，非已学习", "quotas.freeq.assumedTitle") : null }, [line, ...bars]);
  const probe = freeQuotaProbeResults.get(freeKey(row));
  const probeInfo = probe ? freeProbeLabel(probe) : null;
  const lastParts = [];
  if (row.last_status) {
    lastParts.push(t(`HTTP ${row.last_status}`, "quotas.freeq.last.http", row.last_status));
    if (row.last_attempt_at) lastParts.push(timeOnly(row.last_attempt_at));
  } else lastParts.push(t("无记录", "quotas.freeq.last.none"));
  if (probeInfo) lastParts.push(probeInfo.text);
  const lastCell = el("td", {}, [
    el("div", { class: `freeq-last ${probeInfo?.tone || ""}`.trim(), text: lastParts.join(" · ") }),
  ]);
  const busy = freeQuotaProbing && freeQuotaProbeKey === freeKey(row);
  const probeButton = el("button", {
    type: "button",
    class: "btn btn-sm btn-ghost",
    text: busy ? t("探测中…", "quotas.freeq.probing") : t("探测", "quotas.freeq.probe"),
    disabled: freeQuotaProbing || !freeProbeable(row),
  });
  if (!probeButton.disabled) probeButton.addEventListener("click", () => freeProbe(row));
  const actionCell = el("td", {}, [el("div", { class: "freeq-actions" }, [probeButton])]);
  return el("tr", {}, [egressCell, todayCell, lastCell, actionCell]);
}

function freeRender(value, updateStatus = true) {
  if (!value) return;
  const rows = freeRows(value);
  const updated = localTime(value.generated_at || new Date().toISOString());
  $("#freequota-updated").textContent = t(
    `数据为本地估算 · 更新于 ${updated}`,
    "quotas.freeq.updated",
    updated,
  );
  const host = $("#freequota-egresses");
  host.replaceChildren();
  if (value.enabled === false) {
    for (const id of ["freequota-kpi-remaining", "freequota-kpi-used", "freequota-kpi-reset", "freequota-usage-value", "freequota-unknown-value", "freequota-unobserved-value"]) freeSetText(id, "—");
    freeSetText("freequota-kpi-reset-note", "");
    freeSetText("freequota-verdict", t("免费额度估算已关闭。", "quotas.freeq.disabled"));
    for (const id of ["freequota-usage-track", "freequota-unknown-track", "freequota-unobserved-track", "freequota-distribution-track"]) $(`#${id}`)?.replaceChildren();
    $("#freequota-distribution-legend").replaceChildren();
    $("#freequota-unknown-row").classList.add("hidden");
    $("#freequota-unobserved-row").classList.add("hidden");
    if (updateStatus) freeSetStatus("免费额度估算已关闭。", "quotas.freeq.disabled");
    emptyRow(host, 4, t("免费额度估算已关闭。", "quotas.freeq.disabled"));
    freeUpdateProbeAll();
    return;
  }
  freeRenderOverview(value, rows);
  if (!rows.length) {
    $("#freequota-verdict").textContent = t("尚未配置任何出口。", "quotas.freeq.empty");
    if (updateStatus) freeSetStatus("尚未配置任何出口。", "quotas.freeq.empty");
    emptyRow(host, 4, t("尚未配置任何出口。", "quotas.freeq.empty"));
    freeUpdateProbeAll();
    return;
  }
  if (updateStatus) freeSetStatus("", null);
  const order = { proxy: 0, direct: 1, warp: 2 };
  const sorted = [...rows].sort(
    (a, b) =>
      (order[a.kind] ?? 3) - (order[b.kind] ?? 3) ||
      (a.index ?? 0) - (b.index ?? 0) ||
      String(freeLabel(a)).localeCompare(String(freeLabel(b))),
  );
  for (const row of sorted) host.append(freeRow(row, value));
  freeUpdateProbeAll();
}

function freeProbeTargets(value) {
  const rows = freeRows(value);
  const warpLane = value?.warp?.enabled && (value.warp.active ?? 0) > 0 ? 1 : 0;
  return rows.filter(freeProbeable).length + warpLane;
}

function freeUpdateProbeAll() {
  const button = $("#freequota-probe-all");
  if (!button) return;
  const targets = freeProbeTargets(freeQuotaData);
  const usable = Boolean(freeQuotaData) && freeQuotaData.enabled !== false;
  button.disabled = freeQuotaProbing || !usable || targets === 0;
  button.textContent = !freeQuotaProbing
    ? t("探测全部", "quotas.freeq.probeAll")
    : freeQuotaProbeKey === null
      ? t(`探测中… (${targets})`, "quotas.freeq.probingCount", targets)
      : t("探测中…", "quotas.freeq.probing");
}

function freeProbeBody(row) {
  if (row.kind === "warp") return { warp: true };
  return Number.isInteger(row.index) && row.index >= 0
    ? { relay: { index: row.index } }
    : { relay: { name: row.label } };
}

async function freeProbeRequest(body, key) {
  freeQuotaProbing = true;
  freeQuotaProbeKey = key;
  freeUpdateProbeAll();
  freeRender(freeQuotaData, false);
  try {
    const value = await api("/api/free-quotas/probe", {
      method: "POST",
      body: JSON.stringify(body),
    });
    if (!hasSession()) return;
    const probes = Array.isArray(value.probes) ? value.probes : [];
    freeRecordProbes(probes);
    if (value.report) freeQuotaData = value.report;
    const ok = probes.filter((probe) => probe.ok || probe.classification === "ok").length;
    freeSetStatus(`已探测 ${probes.length} 个出口，${ok} 个正常。`, "quotas.freeq.probeDone", probes.length, ok);  } catch (error) {
    freeSetStatus(`探测失败：${error.message}。`, "quotas.freeq.error", error.message);
  } finally {
    freeQuotaProbing = false;
    freeQuotaProbeKey = null;
    freeUpdateProbeAll();
    if (freeQuotaData) freeRender(freeQuotaData, false);
  }
}

async function freeProbe(row) {
  if (freeQuotaProbing || !freeProbeable(row)) return;
  freeSetStatus(`正在探测 ${freeLabel(row)}…`, "quotas.freeq.probeOne", freeLabel(row));
  await freeProbeRequest(freeProbeBody(row), freeKey(row));
}

async function freeProbeAll() {
  if (freeQuotaProbing || !freeQuotaData || freeQuotaData.enabled === false) return;
  freeSetStatus("正在探测全部出口…", "quotas.freeq.probing");
  await freeProbeRequest({ all: true }, null);
}

async function loadFreeQuota() {
  if (freeQuotaLoading) return;
  freeQuotaLoading = true;
  const button = $("#freequota-refresh");
  button.disabled = true;
  button.textContent = t("读取中…", "quotas.freeq.loading");
  try {
    const value = await api("/api/free-quotas");
    if (!hasSession()) return;
    freeQuotaData = value;
    freeRender(value);
  } catch (error) {
    freeSetStatus(`查询失败：${error.message}。`, "quotas.freeq.error", error.message);
  } finally {
    freeQuotaLoading = false;
    button.disabled = false;
    button.textContent = t("刷新估算", "quotas.freeq.refresh");
  }
}

function startFreeQuotaPolling() {
  clearInterval(freeQuotaTimer);
  freeQuotaTimer = setInterval(() => {
    if (!document.hidden && !freeQuotaProbing) loadFreeQuota();
  }, 60000);
}

function stopFreeQuotaPolling() {
  clearInterval(freeQuotaTimer);
  freeQuotaTimer = null;
}

function startPolling() {
  clearInterval(timer);
  timer = setInterval(() => {
    if (!document.hidden) load();
  }, 60000);
}

function stopPolling() {
  clearInterval(timer);
  timer = null;
}

export default {
  id: "model-quotas",
  title: "模型与额度",
  sub: "共享套餐额度与免费模型探测状态。",
  init() {
    bindTabs($("#page-model-quotas"), (id) => {
      activeTab = id;
      if (id === "quota") {
        stopFreeQuotaPolling();
        load();
        startPolling();
      } else if (id === "free") {
        stopPolling();
        stopFreeQuotaPolling();
        loadAvailability();
      } else if (id === "free-quota") {
        stopPolling();
        loadFreeQuota();
        startFreeQuotaPolling();
      }
    });
    $("#quota-refresh").addEventListener("click", load);
    $("#quota-account").addEventListener("change", render);
    $("#quota-model-search").addEventListener("input", render);
    $("#availability-refresh").addEventListener("click", refreshAvailability);
    $("#freequota-refresh").addEventListener("click", loadFreeQuota);
    $("#freequota-probe-all").addEventListener("click", freeProbeAll);
    freeUpdateProbeAll();
    // Only the rows are rebuilt: a button that is mid-request keeps its
    // disabled state and its in-progress label.
    onChange(() => {
      if (!loading) $("#quota-refresh").textContent = t("刷新额度", "quotas.refresh");
      if (!freeQuotaLoading)
        $("#freequota-refresh").textContent = t("刷新估算", "quotas.freeq.refresh");
      if (!freeQuotaProbing) freeUpdateProbeAll();
      const emptyOpt = $("#quota-account")?.querySelector('option[value=""]');
      if (emptyOpt) emptyOpt.textContent = t("尚未配置 Go Key", "quotas.noKey");
      if (activeTab === "quota") render();
      else if (activeTab === "free-quota") {
        if (freeQuotaData) freeRender(freeQuotaData, false);
        if (freeQuotaStatus)
          freeSetStatus(
            freeQuotaStatus.original,
            freeQuotaStatus.key,
            ...freeQuotaStatus.args,
          );
      } else loadAvailability();
    });
  },
  show() {
    if (activeTab === "quota") {
      load();
      startPolling();
    } else if (activeTab === "free-quota") {
      loadFreeQuota();
      startFreeQuotaPolling();
    } else {
      loadAvailability();
    }
  },
  hide() {
    stopPolling();
    stopFreeQuotaPolling();
  },
  refresh() {
    if (activeTab === "quota") load();
    else if (activeTab === "free-quota") loadFreeQuota();
    else loadAvailability();
  },
};
