// SPDX-License-Identifier: AGPL-3.0-or-later

(function () {
  function setActivePanel(menu, name) {
    menu.querySelectorAll("[data-ref-tab]").forEach(function (tab) {
      var active = tab.getAttribute("data-ref-tab") === name;
      tab.classList.toggle("is-active", active);
      tab.setAttribute("aria-selected", active ? "true" : "false");
    });
    menu.querySelectorAll("[data-ref-panel]").forEach(function (panel) {
      panel.hidden = panel.getAttribute("data-ref-panel") !== name;
    });
    var input = menu.querySelector("[data-ref-filter]");
    if (input) {
      input.value = "";
      input.setAttribute("placeholder", name === "tags" ? "Find a tag" : "Find a branch");
      input.setAttribute("aria-label", name === "tags" ? "Find a tag" : "Find a branch");
      filterPanel(menu);
      input.focus();
    }
  }

  function filterPanel(menu) {
    var input = menu.querySelector("[data-ref-filter]");
    var query = input ? input.value.trim().toLowerCase() : "";
    var panel = Array.prototype.find.call(menu.querySelectorAll("[data-ref-panel]"), function (candidate) {
      return !candidate.hidden;
    });
    if (!panel) return;
    var visible = 0;
    panel.querySelectorAll("[data-ref-option]").forEach(function (option) {
      var name = (option.getAttribute("data-ref-name") || option.textContent || "").toLowerCase();
      var match = !query || name.indexOf(query) !== -1;
      option.hidden = !match;
      if (match) visible += 1;
    });
    var empty = panel.querySelector("[data-ref-empty]");
    if (empty) empty.hidden = visible !== 0;
  }

  document.querySelectorAll("[data-ref-menu]").forEach(function (menu) {
    menu.querySelectorAll("[data-ref-tab]").forEach(function (tab) {
      tab.addEventListener("click", function () {
        setActivePanel(menu, tab.getAttribute("data-ref-tab") || "branches");
      });
    });
    var input = menu.querySelector("[data-ref-filter]");
    if (input) {
      input.addEventListener("input", function () { filterPanel(menu); });
    }
    menu.querySelectorAll("[data-ref-close]").forEach(function (close) {
      close.addEventListener("click", function () { menu.open = false; });
    });
    menu.addEventListener("toggle", function () {
      if (menu.open && input) {
        setTimeout(function () { input.focus(); }, 0);
      }
    });
  });
})();
