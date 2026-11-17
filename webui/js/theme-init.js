// Apply the theme before first paint while honoring the script-src policy.
// The theme module owns changes after the console starts.
(() => {
  try {
    const mode = localStorage.getItem("opencode2api.ui.theme") || "auto";
    const dark =
      mode === "dark" ||
      (mode === "auto" && matchMedia("(prefers-color-scheme: dark)").matches);
    document.documentElement.setAttribute("data-theme", dark ? "dark" : "light");
  } catch (_) {}
})();
