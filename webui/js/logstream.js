// Live log stream: SSE connection, bounded buffer and rendering. The
// connection lifecycle is owned by the console shell.
import { $, el, timeOnly, toast } from "./ui.js";
import { t } from "./i18n.js";

let events = [];
let source = null;
let paused = false;
let visible = false;
let renderPending = false;

function render() {
  const level = $("#log-level").value;
  const component = $("#log-component").value.toLowerCase();
  const query = $("#log-search").value.toLowerCase();
  const rows = events
    .filter(
      (item) =>
        (!level || item.level === level) &&
        (!component || (item.component || "").toLowerCase().includes(component)) &&
        (!query || JSON.stringify(item).toLowerCase().includes(query)),
    )
    .slice(-500);
  const host = $("#log-list");
  host.replaceChildren();
  rows.forEach((item) => {
    const levelClass =
      item.level === "error" ? "bad" : item.level === "warn" ? "warn" : item.level === "info" ? "ok" : "";
    host.append(
      el("div", { class: "log" }, [
        el("span", { class: "log-time", text: timeOnly(item.time) }),
        el("span", { class: levelClass, text: item.level }),
        el("span", { class: "log-component", text: item.component || "core" }),
        el("span", { class: "log-message", text: item.message }),
        el("span", { class: "log-fields", text: JSON.stringify(item.fields || {}) }),
      ]),
    );
  });
  if (!rows.length) host.append(el("div", { class: "empty", text: t("等待日志事件…", "logs.waiting") }));
  host.scrollTop = host.scrollHeight;
}

export function connect() {
  disconnect();
  // The server replays its recent ring on every subscribe; start from a clean
  // buffer so a re-login does not duplicate the previous session's events.
  events = [];
  source = new EventSource("/api/logs/stream");
  source.addEventListener("log", (event) => {
    events.push(JSON.parse(event.data));
    if (events.length > 2000) events.shift();
    if (!paused && visible && !renderPending) {
      renderPending = true;
      requestAnimationFrame(() => {
        renderPending = false;
        render();
      });
    }
  });
  source.addEventListener("gap", () => toast(t("较早日志已从内存清除", "logs.gap"), true));
}

export function disconnect() {
  if (source) source.close();
  source = null;
}

// The stream tab reports its visibility so hidden panels skip rendering while
// the buffer keeps filling.
export function setVisible(value) {
  visible = value;
  if (visible) render();
}

export function bind() {
  ["#log-level", "#log-component", "#log-search"].forEach((selector) =>
    $(selector).addEventListener("input", render),
  );
  $("#log-pause").addEventListener("click", () => {
    paused = !paused;
    syncPauseButton();
    if (!paused) render();
  });
}

// The button carries the opposite action of the current state, so it is
// rebuilt on every language switch.
export function syncPauseButton() {
  $("#log-pause").textContent = paused
    ? t("继续", "logs.resume")
    : t("暂停", "logs.pause");
}

export { render };
