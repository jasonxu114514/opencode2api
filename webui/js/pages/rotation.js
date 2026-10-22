// Page: 模型轮转 — built-in sota/sweet, operator-defined static groups and
// query-driven dynamic groups, with ordering, manual selection, a query builder
// with a live membership preview, recent switches and attempts.
import { api, hasSession } from "../api.js";
import { t, onChange } from "../i18n.js";
import { $, el, emptyRow, localTime, toast } from "../ui.js";

const reasons = () => ({
  success: t("成功", "rotation.reason.success"),
  client_canceled: t("调用方取消", "rotation.reason.clientCanceled"),
  quota_or_rate_limit: t("额度不足 / 限流", "rotation.reason.quota"),
  transport_error: t("连接错误", "rotation.reason.transport"),
  first_output_timeout: t("首个输出超时", "rotation.reason.firstOutputTimeout"),
  stream_error: t("输出中断", "rotation.reason.streamError"),
  response_read_error: t("读取响应失败", "rotation.reason.readError"),
  response_conversion_error: t("响应解析或转换失败", "rotation.reason.conversionError"),
  request_conversion_error: t("请求转换失败", "rotation.reason.requestConversion"),
  upstream_error: t("上游错误", "rotation.reason.upstreamError"),
});

const FIELD_LABELS = {
  free: "免费",
  deprecated: "已弃用",
  sota: "SOTA",
  routable: "当前可路由",
  cost_known: "价格已知",
  reasoning: "推理",
  tool_call: "工具调用",
  structured_output: "结构化输出",
  context_window: "上下文窗口",
  max_input: "最大输入",
  max_output: "最大输出",
  "cost.input": "输入价格",
  "cost.output": "输出价格",
  "cost.cache_read": "缓存读取价格",
  "cost.cache_write": "缓存写入价格",
  id: "模型 ID",
  protocol: "协议",
  tier: "Tier",
  input_modalities: "输入模态",
  output_modalities: "输出模态",
};

const OP_LABELS = {
  is: "等于",
  ">=": "≥",
  ">": ">",
  "<=": "≤",
  "<": "<",
  "==": "=",
  "!=": "≠",
  equals: "等于",
  contains: "包含",
  starts_with: "开头是",
  ends_with: "结尾是",
  in: "属于集合",
  has_any: "含任意",
  has_all: "含全部",
  not_has: "不含",
};

const MAX_QUERY_DEPTH = 4;

let data = null;
let dirty = false;
let selected = "";
let preview = null;
let previewTimer = null;
let previewSeq = 0;

// The persisted rotation state serializes nil maps and slices as JSON null;
// normalize them before anything renders against them.
function normalize(value) {
  for (const group of value.groups || []) {
    group.order = group.order || [];
    group.available = group.available || {};
    group.paused = group.paused || {};
    group.switches = group.switches || [];
  }
  value.available_models = value.available_models || [];
  value.query_fields = value.query_fields || [];
  value.sort_fields = value.sort_fields || [];
  value.sort_directions = value.sort_directions || [];
  value.attempts = value.attempts || [];
  return value;
}

function kind(group) {
  if (group.id === "sota") return "sota";
  if (group.id === "sweet") return "sweet";
  return group.dynamic ? "dynamic" : "custom";
}

function label(group) {
  if (group.id === "sota") return "SOTA";
  if (group.id === "sweet") return t("甜点", "rotation.sweet");
  return group.alias || t("自定义分组", "rotation.customDefault");
}

function caption(group) {
  switch (kind(group)) {
    case "sota":
      return t("内置分组，固定包含五个 SOTA 模型，可调整顺序。", "rotation.detail.sota");
    case "sweet":
      return t("内置兜底分组，自动纳入其余 Go / Zen 模型，可调整顺序。", "rotation.detail.sweet");
    case "dynamic":
      return t(
        "查询分组：按条件自动纳入所有匹配模型，模型变化时自动更新，并保留已有优先级。",
        "rotation.detail.dynamic",
      );
    default:
      return t("静态分组：仅轮转下方列出的模型（可来自 Go 或 Zen）。", "rotation.detail.custom");
  }
}

const fieldLabel = (name) => t(FIELD_LABELS[name] || name, `rotation.field.${name}`);
const opLabel = (op) => t(OP_LABELS[op] || op, `rotation.op.${op}`);
const fieldSpec = (name) => data?.query_fields.find((field) => field.name === name);
const domId = (group) => String(group.id).replace(/[^A-Za-z0-9_-]/g, "_");

function toBridge(value) {
  if (Array.isArray(value)) return value.map(String);
  if (value === undefined || value === null || value === "") return [];
  return String(value)
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
}

const parseList = (text) =>
  String(text)
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);

function defaultValue(spec) {
  if (!spec) return "";
  if (spec.kind === "bool") return true;
  if (spec.kind === "number") return 0;
  if (spec.kind === "list") return [];
  return "";
}

function defaultCondition() {
  const spec = data?.query_fields[0] || { name: "reasoning", ops: ["is"], kind: "bool" };
  return { field: spec.name, op: spec.ops[0], value: defaultValue(spec) };
}

const defaultTerm = () => ({ op: "and", condition: defaultCondition() });
const defaultGroupTerm = () => ({ op: "and", group: { terms: [defaultTerm()] } });

function normalizeTerm(term) {
  if (!term || typeof term !== "object") term = {};
  if (term.group && typeof term.group === "object")
    return { op: term.op === "or" ? "or" : "and", group: normalizeQueryNode(term.group) };
  const condition =
    term.condition && typeof term.condition === "object" ? term.condition : defaultCondition();
  return {
    op: term.op === "or" ? "or" : "and",
    condition: { field: condition.field, op: condition.op, value: condition.value },
  };
}

function normalizeQueryNode(node) {
  if (!node || typeof node !== "object") node = {};
  const terms = Array.isArray(node.terms) ? node.terms : [];
  node.terms = terms.map((term) => normalizeTerm(term));
  return node;
}

function normalizeQuery(node) {
  return {
    terms: (node?.terms || []).map((term, index) => {
      const op = index === 0 ? undefined : term.op === "or" ? "or" : "and";
      if (term.group) return { op, group: normalizeQuery(term.group) };
      return {
        op,
        condition: { field: term.condition.field, op: term.condition.op, value: term.condition.value },
      };
    }),
  };
}

function queryConditionCount(node) {
  let total = 0;
  for (const term of node?.terms || []) {
    if (term.condition) total += 1;
    else if (term.group) total += queryConditionCount(term.group);
  }
  return total;
}

async function load() {
  try {
    const value = await api("/api/rotation");
    if (!hasSession()) return;
    data = normalize(value);
    dirty = false;
    selected = data.groups[0]?.id || "";
    render();
  } catch (error) {
    toast(error.message, true);
  }
}

function select(id) {
  selected = id;
  render();
}

function render() {
  if (!data) return;
  $("#rotation-status").textContent =
    data.persistence_error ||
    t(
      "状态已更新 · 自定义与内置分组独立轮转，所有调用方共享当前位置。",
      "rotation.status",
    );
  $("#rotation-timeout").value = data.first_output_seconds;
  renderList();
  renderDetail();
  renderAttempts();
}

function renderList() {
  const host = $("#rotation-list");
  host.replaceChildren();
  for (const group of data.groups) {
    const paused = group.order.filter(
      (model) => group.paused?.[model] && new Date(group.paused[model]) > new Date(),
    ).length;
    const kindTag = kind(group);
    const tag = t(
      kindTag === "dynamic" ? "查询" : kindTag === "custom" ? "静态" : kindTag === "sota" ? "内置" : "内置兜底",
      `rotation.tag.${kindTag}`,
    );
    const meta = [
      tag,
      group.current || t("等待可用模型", "rotation.waitingModel"),
      t(`${group.order.length} 个模型`, "rotation.modelCount", group.order.length),
    ];
    if (paused) meta.push(t(`${paused} 暂停`, "rotation.pausedCount", paused));
    host.append(
      el(
        "button",
        {
          type: "button",
          class: `rotation-item${group.id === selected ? " active" : ""}`,
          onclick: () => select(group.id),
        },
        [
          el("span", { class: "rotation-item-name", text: label(group) }),
          el("span", {
            class: "rotation-item-alias mono",
            text: t(`别名 ${group.alias}`, "rotation.itemAlias", group.alias),
          }),
          el("span", { class: "rotation-item-meta", text: meta.join(" · ") }),
        ],
      ),
    );
  }
  if (!data.groups.length) host.textContent = t("暂无分组", "rotation.noGroups");
}

const cell = (value, cls) => el("td", { class: cls, text: value });

function modelTable(group) {
  const groupKind = kind(group);
  const reorder = groupKind !== "dynamic";
  const removable = groupKind === "custom";
  const table = el("table", { class: "table" }, [
    el("thead", {}, [
      el(
        "tr",
        {},
        [
          t("顺序", "rotation.col.order"),
          t("模型", "common.model"),
          t("状态 / 恢复时间", "rotation.col.state"),
          t("操作", "common.action"),
        ].map((text) => el("th", { text })),
      ),
    ]),
  ]);
  const body = el("tbody");
  table.append(body);
  const pins = new Set(group.pin || []);
  group.order.forEach((model, index) => {
    const row = el("tr", {}, [cell(String(index + 1), "num"), cell((pins.has(model) ? "★ " : "") + model, "mono")]);
    const until = group.paused?.[model];
    const paused = until && new Date(until) > new Date();
    row.append(
      cell(
        !group.available?.[model]
          ? t("已下架 / 暂不可用", "rotation.unavailable")
          : paused
            ? t(`暂停至 ${localTime(until)}`, "rotation.pausedUntil", localTime(until))
            : model === group.current
              ? t("当前使用", "rotation.inUse")
              : t("可用", "state.available"),
      ),
    );
    const actions = el("div", { class: "rotation-actions" });
    if (reorder) {
      for (const [text, delta] of [
        [t("上移", "rotation.moveUp"), -1],
        [t("下移", "rotation.moveDown"), 1],
      ]) {
        const button = el("button", {
          type: "button",
          class: "btn btn-sm btn-ghost",
          text,
          "aria-label": `${model} ${text}`,
          disabled: index + delta < 0 || index + delta >= group.order.length,
        });
        button.addEventListener("click", () => {
          [group.order[index], group.order[index + delta]] = [
            group.order[index + delta],
            group.order[index],
          ];
          data.first_output_seconds = Number($("#rotation-timeout").value);
          dirty = true;
          render();
        });
        actions.append(button);
      }
    }
    if (removable) {
      const remove = el("button", {
        type: "button",
        class: "btn btn-sm btn-ghost",
        text: t("移除", "rotation.remove"),
        "aria-label": t(`移除 ${model}`, "rotation.removeModel", model),
        disabled: group.order.length <= 1,
      });
      remove.addEventListener("click", () => {
        group.order.splice(index, 1);
        dirty = true;
        render();
      });
      actions.append(remove);
    }
    const pick = el("button", {
      type: "button",
      class: "btn btn-sm btn-ghost",
      text: t("设为当前", "rotation.setCurrent"),
      "aria-label": t(`使用 ${model}`, "rotation.useModel", model),
      disabled: !group.available?.[model],
    });
    pick.addEventListener("click", async () => {
      pick.disabled = true;
      try {
        const result = normalize(
          await api("/api/rotation/current", {
            method: "POST",
            body: JSON.stringify({ group: group.id, model }),
          }),
        );
        // Keep unsaved alias/order edits while refreshing operational state.
        for (const item of data.groups) {
          const live = result.groups.find((entry) => entry.id === item.id);
          if (!live) continue;
          Object.assign(item, { current: live.current, paused: live.paused, switches: live.switches });
        }
        data.persistence_error = result.persistence_error;
        render();
        toast(t("当前模型已切换", "toast.rotation.switched"));
      } catch (error) {
        pick.disabled = false;
        toast(error.message, true);
      }
    });
    actions.append(pick);
    row.append(el("td", {}, [actions]));
    body.append(row);
  });
  if (!group.order.length)
    emptyRow(body, 4, group.dynamic ? t("暂无匹配模型", "rotation.table.emptyDynamic") : t("等待添加模型", "rotation.waitingAdd"));
  return el("div", { class: "table-wrap" }, [table]);
}

function addModel(group) {
  const wrap = el("div", { class: "actions rotation-add" });
  const used = new Set(group.order);
  const options = data.available_models.filter((model) => !used.has(model));
  const picker = el("select", { id: `rotation-add-${domId(group)}` });
  for (const model of options) picker.append(el("option", { value: model, text: model }));
  const button = el("button", {
    type: "button",
    class: "btn btn-sm btn-ghost",
    text: t("添加模型", "rotation.addModel"),
    disabled: !options.length,
  });
  button.addEventListener("click", () => {
    if (!picker.value) return;
    group.order.push(picker.value);
    dirty = true;
    render();
  });
  wrap.append(picker, button);
  return wrap;
}

function dynamicEditor(group) {
  group.query = normalizeQueryNode(group.query);
  if (!group.query.terms.length) group.query.terms.push(defaultTerm());
  if (!Array.isArray(group.pin)) group.pin = [];
  const wrap = el("div", { class: "rotation-query" }, [
    el("h3", { text: t("查询条件", "rotation.query.title") }),
    querySequence(group, group.query, 0, true),
    sortBlock(group),
    pinsBlock(group),
  ]);
  const previewHost = el("div", { class: "rotation-preview", id: `rotation-preview-${domId(group)}` });
  wrap.append(previewHost);
  return wrap;
}

function querySequence(group, node, depth, isRoot) {
  const box = el("div", { class: `rotation-sequence${isRoot ? " rotation-sequence-root" : ""}` });
  node.terms.forEach((term, index) => {
    if (index > 0) box.append(operatorRow(group, term));
    box.append(
      term.group ? groupTerm(group, node, term, index, depth) : conditionCard(group, node, term, index),
    );
  });
  box.append(sequenceActions(group, node, depth));
  return box;
}

function operatorRow(group, term) {
  const select = el("select");
  for (const [value, text] of [
    ["and", t("并且 (AND)", "rotation.op.and")],
    ["or", t("或者 (OR)", "rotation.op.or")],
  ])
    select.append(el("option", { value, text }));
  select.value = term.op === "or" ? "or" : "and";
  select.addEventListener("change", () => {
    term.op = select.value;
    dirty = true;
    schedulePreview(group);
  });
  return el("div", { class: "rotation-operator" }, [select]);
}

function groupTerm(group, node, term, index, depth) {
  const remove = el("button", {
    type: "button",
    class: "btn btn-sm btn-ghost",
    text: t("删除分组", "rotation.group.remove"),
  });
  remove.addEventListener("click", () => {
    node.terms.splice(index, 1);
    if (!node.terms.length) node.terms.push(defaultTerm());
    dirty = true;
    render();
  });
  return el("div", { class: "rotation-subgroup" }, [
    el("div", { class: "rotation-subgroup-label" }, [
      el("span", { text: t("分组 · 开始", "rotation.group.start") }),
      remove,
    ]),
    querySequence(group, term.group, depth + 1, false),
    el("div", { class: "rotation-subgroup-label", text: t("分组 · 结束", "rotation.group.end") }),
  ]);
}

function sequenceActions(group, node, depth) {
  const addCondition = el("button", {
    type: "button",
    class: "btn btn-sm btn-ghost",
    text: t("添加条件", "rotation.query.addCondition"),
  });
  addCondition.addEventListener("click", () => {
    node.terms.push(defaultTerm());
    dirty = true;
    render();
  });
  const addGroup = el("button", {
    type: "button",
    class: "btn btn-sm btn-ghost",
    text: t("添加分组", "rotation.query.addGroup"),
    disabled: depth >= MAX_QUERY_DEPTH,
  });
  addGroup.addEventListener("click", () => {
    node.terms.push(defaultGroupTerm());
    dirty = true;
    render();
  });
  return el("div", { class: "actions rotation-sequence-actions" }, [addCondition, addGroup]);
}

function conditionCard(group, node, term, index) {
  const condition = term.condition;
  const field = el("select");
  for (const spec of data.query_fields) field.append(el("option", { value: spec.name, text: fieldLabel(spec.name) }));
  field.value = condition.field;
  const operator = el("select");
  const spec = fieldSpec(condition.field);
  for (const op of spec?.ops || []) operator.append(el("option", { value: op, text: opLabel(op) }));
  operator.value = condition.op;
  field.addEventListener("change", () => {
    const next = fieldSpec(field.value);
    condition.field = field.value;
    condition.op = next?.ops?.[0] || "is";
    condition.value = defaultValue(next);
    dirty = true;
    render();
  });
  operator.addEventListener("change", () => {
    condition.op = operator.value;
    if (spec?.kind === "string") condition.value = condition.op === "in" ? [] : "";
    dirty = true;
    render();
  });
  const remove = el("button", {
    type: "button",
    class: "btn btn-sm btn-ghost",
    text: t("删除", "rotation.condition.delete"),
  });
  remove.addEventListener("click", () => {
    node.terms.splice(index, 1);
    if (!node.terms.length) node.terms.push(defaultTerm());
    dirty = true;
    render();
  });
  const card = el("div", { class: "rotation-condition-card" }, [
    field,
    operator,
    valueEditor(group, condition),
  ]);
  const hint = fieldHint(condition);
  if (hint) card.append(hint);
  card.append(remove);
  return card;
}

function valueEditor(group, condition) {
  const spec = fieldSpec(condition.field);
  const fieldKind = spec?.kind || "string";
  const values = spec?.values || [];
  const host = el("span", { class: "rotation-value" });
  if (fieldKind === "bool") {
    const select = el("select");
    for (const value of ["true", "false"])
      select.append(el("option", { value, text: value === "true" ? t("是", "rotation.value.yes") : t("否", "rotation.value.no") }));
    select.value = String(condition.value === true);
    select.addEventListener("change", () => {
      condition.value = select.value === "true";
      dirty = true;
      schedulePreview(group);
    });
    host.append(select);
    return host;
  }
  const isList = fieldKind === "list" || condition.op === "in";
  if (values.length && isList) {
    host.append(valueCheckboxes(group, condition, values));
    return host;
  }
  if (values.length && condition.op === "equals") {
    host.append(valueDropdown(group, condition, values));
    return host;
  }
  const input = el("input", {
    type: fieldKind === "number" ? "number" : "text",
    step: fieldKind === "number" ? "any" : null,
    placeholder: spec?.hint || (isList ? t("多个值用逗号分隔", "rotation.value.commaList") : ""),
    value: isList ? toBridge(condition.value).join(", ") : condition.value ?? "",
  });
  input.addEventListener("input", () => {
    if (isList) condition.value = parseList(input.value);
    else if (fieldKind === "number") condition.value = input.value === "" ? 0 : Number(input.value);
    else condition.value = input.value;
    dirty = true;
    schedulePreview(group);
  });
  host.append(input);
  return host;
}

function valueDropdown(group, condition, values) {
  const select = el("select");
  select.append(el("option", { value: "", text: t("请选择", "rotation.value.choose") }));
  for (const value of values) select.append(el("option", { value, text: value }));
  select.value = typeof condition.value === "string" ? condition.value : "";
  select.addEventListener("change", () => {
    condition.value = select.value;
    dirty = true;
    schedulePreview(group);
  });
  return select;
}

function valueCheckboxes(group, condition, values) {
  const wrap = el("span", { class: "rotation-options" });
  const selected = new Set(toBridge(condition.value));
  for (const value of values) {
    const box = el("input", { type: "checkbox", checked: selected.has(value) });
    box.addEventListener("change", () => {
      const current = new Set(toBridge(condition.value));
      if (box.checked) current.add(value);
      else current.delete(value);
      condition.value = values.filter((item) => current.has(item));
      dirty = true;
      schedulePreview(group);
    });
    wrap.append(el("label", { class: "rotation-option" }, [box, el("span", { text: value })]));
  }
  return wrap;
}

function fieldHint(condition) {
  const hint = fieldSpec(condition.field)?.hint;
  return hint ? el("span", { class: "rotation-hint", text: hint }) : null;
}

function sortBlock(group) {
  const field = el("select");
  field.append(el("option", { value: "", text: t("保持顺序（新模型排在末尾）", "rotation.sort.none") }));
  for (const spec of data.sort_fields) field.append(el("option", { value: spec.name, text: fieldLabel(spec.name) }));
  field.value = group.sort?.field || "";
  const direction = el("select");
  for (const value of data.sort_directions.filter((item) => item !== "none"))
    direction.append(el("option", { value, text: value === "desc" ? t("降序", "rotation.sort.desc") : t("升序", "rotation.sort.asc") }));
  direction.value = group.sort?.direction === "desc" ? "desc" : "asc";
  direction.disabled = !field.value;
  field.addEventListener("change", () => {
    group.sort = field.value ? { field: field.value, direction: direction.value } : null;
    dirty = true;
    render();
  });
  direction.addEventListener("change", () => {
    if (group.sort) {
      group.sort.direction = direction.value;
      dirty = true;
      schedulePreview(group);
    }
  });
  return el("div", { class: "rotation-sort" }, [
    el("h3", { text: t("排序（可选）", "rotation.sort.title") }),
    el("div", { class: "actions" }, [field, direction]),
  ]);
}

function pinsBlock(group) {
  const list = el("div", { class: "rotation-pin-list" });
  group.pin.forEach((model, index) => {
    const actions = el("span", { class: "rotation-actions" });
    for (const [text, delta] of [
      [t("上移", "rotation.moveUp"), -1],
      [t("下移", "rotation.moveDown"), 1],
    ]) {
      const button = el("button", {
        type: "button",
        class: "btn btn-sm btn-ghost",
        text,
        disabled: index + delta < 0 || index + delta >= group.pin.length,
      });
      button.addEventListener("click", () => {
        [group.pin[index], group.pin[index + delta]] = [group.pin[index + delta], group.pin[index]];
        dirty = true;
        render();
      });
      actions.append(button);
    }
    const remove = el("button", {
      type: "button",
      class: "btn btn-sm btn-ghost",
      text: t("取消", "rotation.pins.cancel"),
    });
    remove.addEventListener("click", () => {
      group.pin.splice(index, 1);
      dirty = true;
      render();
    });
    actions.append(remove);
    list.append(el("div", { class: "rotation-pin" }, [el("span", { class: "mono", text: model }), actions]));
  });
  if (!group.pin.length)
    list.append(el("p", { class: "section-caption", text: t("未设置固定优先模型。", "rotation.pins.empty") }));
  const picker = el("select", { id: `rotation-pin-add-${domId(group)}` });
  const add = el("button", {
    type: "button",
    class: "btn btn-sm btn-ghost",
    text: t("置顶", "rotation.pins.add"),
  });
  add.addEventListener("click", () => {
    if (!picker.value) return;
    group.pin.push(picker.value);
    dirty = true;
    render();
  });
  return el("div", { class: "rotation-pins" }, [
    el("h3", { text: t("固定优先", "rotation.pins.title") }),
    list,
    el("div", { class: "actions rotation-add" }, [picker, add]),
  ]);
}

function renderDetail() {
  const host = $("#rotation-detail");
  host.replaceChildren();
  const group = data.groups.find((item) => item.id === selected);
  if (!group) {
    host.append(
      el("section", { class: "section" }, [
        el("p", { class: "section-caption", text: t("请选择一个分组查看详情。", "rotation.detail.none") }),
      ]),
    );
    return;
  }
  const groupKind = kind(group);
  const aliasInput = el("input", {
    id: `rotation-alias-${group.id}`,
    value: group.alias,
    required: true,
    maxlength: 64,
  });
  aliasInput.addEventListener("input", () => {
    group.alias = aliasInput.value;
    dirty = true;
    renderList();
  });
  const head = el("div", { class: "section-head" }, [
    el("div", {}, [
      el("h2", { class: "section-title", text: label(group) }),
      el("p", { class: "section-caption", text: caption(group) }),
    ]),
  ]);
  if (groupKind === "custom" || groupKind === "dynamic") {
    const remove = el("button", {
      type: "button",
      class: "btn btn-sm btn-danger",
      text: t("删除分组", "rotation.removeGroup"),
    });
    remove.addEventListener("click", () => {
      data.groups = data.groups.filter((item) => item.id !== group.id);
      dirty = true;
      select(data.groups[0]?.id || "");
    });
    head.append(remove);
  }
  const reasonMap = reasons();
  const recent = el("div", { class: "rotation-switches" }, [
    el("h3", { text: t("最近切换", "rotation.recentSwitches") }),
  ]);
  for (const item of (group.switches || []).slice(-5).reverse()) {
    recent.append(
      el("p", {
        text: `${localTime(item.time)} · ${item.from} → ${item.to} · ${reasonMap[item.reason] || item.reason}`,
      }),
    );
  }
  if (!group.switches?.length)
    recent.append(el("p", { text: t("尚无切换记录", "rotation.noSwitches") }));

  const section = el("section", { class: "section" }, [
    head,
    el("div", { class: "field" }, [
      el("label", { for: `rotation-alias-${group.id}`, text: t("调用别名", "rotation.aliasLabel") }),
      aliasInput,
    ]),
    el("p", {
      class: "rotation-current",
      text: t(
        `当前模型：${group.current || t("等待可用模型", "rotation.waitingModel")}`,
        "rotation.current",
        group.current || t("等待可用模型", "rotation.waitingModel"),
      ),
    }),
  ]);
  if (groupKind === "custom") section.append(addModel(group));
  if (groupKind === "dynamic") section.append(dynamicEditor(group));
  section.append(modelTable(group), recent);
  host.append(section);
  if (groupKind === "dynamic") schedulePreview(group);
}

function schedulePreview(group) {
  if (!group.dynamic || !data) return;
  clearTimeout(previewTimer);
  preview = { group: group.id, loading: true, models: [], count: 0, error: "" };
  renderPreview(group);
  previewTimer = setTimeout(() => runPreview(group), 250);
}

async function runPreview(group) {
  const query = normalizeQuery(group.query);
  if (!queryConditionCount(query)) {
    preview = { group: group.id, loading: false, models: [], count: 0, error: t("请先添加条件", "rotation.preview.addCondition") };
    renderPreview(group);
    return;
  }
  const seq = ++previewSeq;
  try {
    const body = { query };
    if (group.sort?.field) body.sort = group.sort;
    if (group.pin?.length) body.pin = group.pin;
    const value = await api("/api/rotation/preview", { method: "POST", body: JSON.stringify(body) });
    if (seq !== previewSeq || !data) return;
    preview = { group: group.id, loading: false, models: value.models || [], count: value.count || 0, error: "" };
  } catch (error) {
    if (seq !== previewSeq) return;
    preview = { group: group.id, loading: false, models: [], count: 0, error: error.message };
  }
  renderPreview(group);
}

function factSummary(model) {
  const parts = [model.id];
  if (model.tier?.length) parts.push(model.tier.join("/"));
  parts.push(model.reasoning ? t("推理", "rotation.fact.reasoning") : t("非推理", "rotation.fact.nonReasoning"));
  if (model.context_window) parts.push(`ctx ${model.context_window}`);
  if (model.cost_input !== undefined && model.cost_input !== null) parts.push(`$${model.cost_input}/M`);
  return parts.join(" · ");
}

function renderPreview(group) {
  const host = document.getElementById(`rotation-preview-${domId(group)}`);
  if (host) {
    host.replaceChildren();
    const current = preview?.group === group.id ? preview : null;
    if (!current || current.loading) {
      host.append(el("h3", { text: t("匹配结果（计算中…）", "rotation.preview.computing") }));
    } else if (current.error) {
      host.append(
        el("h3", { text: t("匹配结果", "rotation.preview.title") }),
        el("p", { class: "section-caption", text: current.error }),
      );
    } else {
      host.append(el("h3", { text: t(`匹配结果（${current.count}）`, "rotation.preview.count", current.count) }));
      if (!current.models.length) {
        host.append(el("p", { class: "section-caption", text: t("没有模型匹配当前条件", "rotation.preview.none") }));
      } else {
        const list = el("ol", { class: "rotation-preview-list" });
        for (const model of current.models) list.append(el("li", { class: "mono", text: factSummary(model) }));
        host.append(list);
      }
    }
  }
  refreshPinCandidates(group);
}

function refreshPinCandidates(group) {
  const select = document.getElementById(`rotation-pin-add-${domId(group)}`);
  if (!select) return;
  const pinned = new Set(group.pin || []);
  const matched = preview?.group === group.id && !preview.loading ? preview.models : [];
  select.replaceChildren();
  for (const model of matched) {
    if (pinned.has(model.id)) continue;
    select.append(el("option", { value: model.id, text: model.id }));
  }
}

function renderAttempts() {
  const reasonMap = reasons();
  const attempts = $("#rotation-attempts");
  attempts.replaceChildren();
  for (const item of (data.attempts || []).slice().reverse()) {
    attempts.append(
      el("tr", {}, [
        el("td", { class: "mono", text: localTime(item.time) }),
        el("td", { class: "mono", text: item.request }),
        el("td", { text: item.alias }),
        el("td", { class: "mono", text: item.model }),
        el("td", { class: "num", text: String(item.number) }),
        el("td", { text: reasonMap[item.outcome] || item.outcome }),
      ]),
    );
  }
  if (!data.attempts?.length)
    emptyRow(attempts, 6, t("调用分组别名后显示最近 100 次尝试", "rotation.attemptsEmpty"));
}

async function save(event) {
  event.preventDefault();
  if (!data) return;
  const button = $("#rotation-save");
  button.disabled = true;
  try {
    const body = { first_output_seconds: Number($("#rotation-timeout").value) || 30 };
    for (const group of data.groups) {
      const entry = { alias: group.alias.trim() };
      if (group.id === "sota" || group.id === "sweet") {
        entry.order = group.order.slice();
        body[group.id] = entry;
      } else if (group.dynamic) {
        entry.query = normalizeQuery(group.query);
        if (group.sort?.field) entry.sort = { field: group.sort.field, direction: group.sort.direction || "asc" };
        if (group.pin?.length) entry.pin = group.pin.slice();
        (body.custom ||= []).push(entry);
      } else {
        entry.order = group.order.slice();
        (body.custom ||= []).push(entry);
      }
    }
    data = normalize(await api("/api/rotation", { method: "PUT", body: JSON.stringify(body) }));
    dirty = false;
    if (!data.groups.some((group) => group.id === selected)) selected = data.groups[0]?.id || "";
    render();
    toast(t("模型轮转配置已保存并生效", "toast.rotation.saved"));
  } catch (error) {
    toast(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function addGroup(dynamic) {
  if (!data) return;
  const aliases = new Set(data.groups.map((group) => group.alias));
  const base = dynamic ? "query" : "group";
  let name = `${base}-1`;
  for (let i = 2; aliases.has(name); i++) name = `${base}-${i}`;
  const group = {
    id: `custom:${name}`,
    alias: name,
    order: [],
    current: "",
    generation: 0,
    paused: {},
    available: {},
    switches: [],
  };
  if (dynamic) {
    group.dynamic = true;
    group.query = { terms: [defaultTerm()] };
    group.sort = null;
    group.pin = [];
  } else {
    const used = new Set(data.groups.flatMap((item) => item.order));
    const first = data.available_models.find((model) => !used.has(model)) || "";
    group.order = first ? [first] : [];
    group.available = first ? { [first]: true } : {};
    if (!first) toast(t("没有可用的空闲模型，请先为该分组添加模型", "toast.rotation.groupEmpty"), true);
  }
  data.groups.push(group);
  dirty = true;
  select(group.id);
}

export default {
  id: "rotation",
  title: "模型轮转",
  sub: "共享当前模型、故障切换与尝试记录。",
  init() {
    $("#rotation-refresh").addEventListener("click", () => {
      if (dirty) {
        toast(t("有未保存的修改，请先保存后刷新", "toast.rotation.dirty"), true);
        return;
      }
      load();
    });
    $("#rotation-timeout").addEventListener("input", () => {
      dirty = true;
    });
    $("#rotation-add").addEventListener("click", () => addGroup(false));
    $("#rotation-add-dynamic").addEventListener("click", () => addGroup(true));
    $("#rotation-form").addEventListener("submit", save);
    onChange(() => {
      if (data) {
        data.first_output_seconds = Number($("#rotation-timeout").value);
        render();
      }
    });
  },
  show: load,
};
