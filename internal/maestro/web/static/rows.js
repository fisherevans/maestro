// Makes any <tr data-href="..."> clickable. Click anywhere on the row
// navigates to the URL. Clicks on nested anchors still work (they win
// because of the early return on tag check), so users can still
// shift/cmd-click links inside a row without triggering row navigation.
document.addEventListener("DOMContentLoaded", function () {
  document.querySelectorAll("tr[data-href]").forEach(function (row) {
    row.addEventListener("click", function (e) {
      // Ignore clicks that landed on an interactive child element.
      var tag = (e.target.tagName || "").toUpperCase();
      if (tag === "A" || tag === "BUTTON" || tag === "INPUT" || tag === "LABEL") return;
      if (e.target.closest("a, button, input, label")) return;
      // Cmd/ctrl/middle-click opens in a new tab.
      if (e.metaKey || e.ctrlKey || e.button === 1) {
        window.open(row.dataset.href, "_blank");
        return;
      }
      window.location = row.dataset.href;
    });
  });
});
