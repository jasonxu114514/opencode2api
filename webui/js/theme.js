// Theme handling: an auto/light/dark cycle persisted per browser. The inline
// head script applies the stored choice before first paint; this module owns
// the topbar toggle and keeps "auto" tracking the system preference.
import { t, onChange } from "./i18n.js";

const STORAGE_KEY = "opencode2api.ui.theme";
const MODES = ["auto", "light", "dark"];
const media = window.matchMedia("(prefers-color-scheme: dark)");

const stored = () => {
  try {
    return localStorage.getItem(STORAGE_KEY) || "";
  } catch {
    return "";
  }
};

let mode = MODES.includes(stored()) ? stored() : "auto";

const isDark = () => mode === "dark" || (mode === "auto" && media.matches);

function label() {
  if (mode === "light") return t("浅色", "theme.light");
  if (mode === "dark") return t("深色", "theme.dark");
  return t("跟随系统", "theme.auto");
}

function apply() {
  document.documentElement.setAttribute("data-theme", isDark() ? "dark" : "light");
  const button = document.getElementById("theme-toggle");
  if (button) {
    button.textContent = label();
    button.title = t("切换主题", "theme.toggle");
  }
}

// init binds the toggle and re-renders the label after language switches.
// Cycles auto → light → dark so system-dark users keep their look until they
// make an explicit choice.
export function init() {
  const button = document.getElementById("theme-toggle");
  if (button)
    button.addEventListener("click", () => {
      mode = MODES[(MODES.indexOf(mode) + 1) % MODES.length];
      try {
        localStorage.setItem(STORAGE_KEY, mode);
      } catch (_) {}
      apply();
    });
  media.addEventListener("change", apply);
  onChange(apply);
  apply();
}
