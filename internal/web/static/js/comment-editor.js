// SPDX-License-Identifier: AGPL-3.0-or-later

(function () {
  "use strict";

  const slashCommands = [
    {
      label: "Alerts",
      description: "Add a markdown alert to emphasize important information",
      snippet: "> [!NOTE]\n> Important information\n"
    },
    {
      label: "Code block",
      description: "Insert a code block formatted for a chosen syntax",
      snippet: "```\ncode\n```\n"
    },
    {
      label: "Details",
      description: "Add a details tag to hide content behind a visible heading",
      snippet: "<details>\n<summary>Summary</summary>\n\nDetails\n</details>\n"
    },
    {
      label: "Saved replies",
      description: "Insert one of your saved replies",
      action: "saved"
    },
    {
      label: "Table",
      description: "Add markdown table",
      snippet: "| Column | Value |\n| --- | --- |\n| Item | Value |\n"
    }
  ];

  function initEditor(root) {
    const form = root.querySelector("form") || root.closest("form");
    const box = root.querySelector(".shithub-comment-editor-box");
    const textarea = root.querySelector("[data-comment-textarea]");
    const writePane = root.querySelector("[data-comment-write-pane]");
    const previewPane = root.querySelector("[data-comment-preview-pane]");
    const suggestions = root.querySelector("[data-comment-suggestions]");
    const submit = root.querySelector("[data-comment-submit]");
    const savedDialog = root.querySelector("[data-comment-saved-dialog]");
    const fileInput = root.querySelector("[data-comment-file-input]");
    const fileList = root.querySelector("[data-comment-file-list]");
    const config = readConfig(root);
    let activeToken = null;
    let previewDirty = true;

    if (!form || !box || !textarea) return;

    function setTab(tab) {
      root.querySelectorAll("[data-comment-tab]").forEach(function (button) {
        const active = button.dataset.commentTab === tab;
        button.classList.toggle("is-active", active);
        button.setAttribute("aria-selected", active ? "true" : "false");
      });
      if (writePane) writePane.hidden = tab !== "write";
      if (previewPane) previewPane.hidden = tab !== "preview";
      if (tab === "preview") renderPreview();
      if (tab === "write") textarea.focus();
    }

    function updateSubmit() {
      if (submit) submit.disabled = textarea.value.trim() === "";
    }

    async function renderPreview() {
      if (!previewPane || !previewDirty) return;
      if (textarea.value.trim() === "") {
        previewPane.innerHTML = "<p class=\"shithub-editor-preview-empty\">Nothing to preview.</p>";
        previewDirty = false;
        return;
      }
      const csrf = form.querySelector("input[name='csrf_token']");
      const body = new URLSearchParams();
      body.set("csrf_token", csrf ? csrf.value : "");
      body.set("content", textarea.value);
      body.set("ref", root.dataset.previewRef || "");
      body.set("path", "comment.md");
      previewPane.innerHTML = "<p class=\"shithub-editor-preview-empty\">Rendering preview...</p>";
      try {
        const response = await fetch(root.dataset.previewUrl || "", {
          method: "POST",
          headers: {
            "Content-Type": "application/x-www-form-urlencoded",
            "X-Requested-With": "XMLHttpRequest"
          },
          body: body.toString()
        });
        previewPane.innerHTML = response.ok ? await response.text() : "<p class=\"shithub-editor-preview-empty\">Preview failed.</p>";
      } catch (error) {
        previewPane.innerHTML = "<p class=\"shithub-editor-preview-empty\">Preview failed.</p>";
      }
      previewDirty = false;
    }

    function replaceSelection(before, after, fallback) {
      const start = textarea.selectionStart;
      const end = textarea.selectionEnd;
      const selected = textarea.value.slice(start, end) || fallback;
      const replacement = before + selected + after;
      textarea.setRangeText(replacement, start, end, "select");
      textarea.selectionStart = start + before.length;
      textarea.selectionEnd = start + before.length + selected.length;
      afterEdit();
    }

    function prefixSelection(prefix) {
      const start = textarea.selectionStart;
      const end = textarea.selectionEnd;
      const selected = textarea.value.slice(start, end) || "";
      const source = selected || currentLineText();
      const replacement = source.split("\n").map(function (line) {
        return line.trim() === "" ? prefix.trimEnd() : prefix + line;
      }).join("\n");
      if (selected) {
        textarea.setRangeText(replacement, start, end, "end");
      } else {
        const line = currentLineRange();
        textarea.setRangeText(replacement, line.start, line.end, "end");
      }
      afterEdit();
    }

    function currentLineRange() {
      const pos = textarea.selectionStart;
      const before = textarea.value.slice(0, pos);
      const after = textarea.value.slice(pos);
      const lineStart = before.lastIndexOf("\n") + 1;
      let lineEnd = after.indexOf("\n");
      lineEnd = lineEnd === -1 ? textarea.value.length : pos + lineEnd;
      return { start: lineStart, end: lineEnd };
    }

    function currentLineText() {
      const line = currentLineRange();
      return textarea.value.slice(line.start, line.end);
    }

    function insertText(text) {
      const start = textarea.selectionStart;
      const end = textarea.selectionEnd;
      textarea.setRangeText(text, start, end, "end");
      afterEdit();
    }

    function afterEdit() {
      previewDirty = true;
      updateSubmit();
      hideSuggestions();
      textarea.focus();
    }

    function runAction(action) {
      switch (action) {
      case "heading":
        prefixSelection("### ");
        break;
      case "bold":
        replaceSelection("**", "**", "bold text");
        break;
      case "italic":
        replaceSelection("*", "*", "italic text");
        break;
      case "quote":
        prefixSelection("> ");
        break;
      case "code":
        if (textarea.value.slice(textarea.selectionStart, textarea.selectionEnd).includes("\n")) {
          replaceSelection("```\n", "\n```", "code");
        } else {
          replaceSelection("`", "`", "code");
        }
        break;
      case "link":
        replaceSelection("[", "](url)", "link text");
        break;
      case "list":
        prefixSelection("- ");
        break;
      case "ordered-list":
        prefixSelection("1. ");
        break;
      case "task-list":
        prefixSelection("- [ ] ");
        break;
      case "mention":
        showSuggestions("mention", "");
        break;
      case "reference":
        showSuggestions("reference", "");
        break;
      case "fullscreen":
        box.classList.toggle("shithub-comment-editor-fullscreen");
        root.querySelector("[data-comment-action='fullscreen']")?.classList.toggle("is-active", box.classList.contains("shithub-comment-editor-fullscreen"));
        textarea.focus();
        break;
      }
    }

    function detectToken() {
      const pos = textarea.selectionStart;
      const before = textarea.value.slice(0, pos);
      const lineStart = before.lastIndexOf("\n") + 1;
      const line = before.slice(lineStart);
      const slash = line.match(/^\/([A-Za-z-]*)$/);
      if (slash) {
        return { kind: "slash", query: slash[1].toLowerCase(), start: lineStart, end: pos };
      }
      const match = before.match(/(^|[\s([])([@#])([A-Za-z0-9_.-]*)$/);
      if (!match) return null;
      const marker = match[2];
      return {
        kind: marker === "@" ? "mention" : "reference",
        query: match[3].toLowerCase(),
        start: pos - marker.length - match[3].length,
        end: pos
      };
    }

    function showDetectedSuggestions() {
      const token = detectToken();
      if (!token) {
        hideSuggestions();
        return;
      }
      showSuggestions(token.kind, token.query, token);
    }

    function showSuggestions(kind, query, token) {
      if (!suggestions) return;
      activeToken = token || {
        kind: kind,
        query: query || "",
        start: textarea.selectionStart,
        end: textarea.selectionEnd
      };
      let html = "";
      let items = [];
      suggestions.classList.toggle("is-slash", kind === "slash");
      if (kind === "mention") {
        items = config.mentions.filter(function (item) {
          return item.username.toLowerCase().includes(query) || (item.displayName || "").toLowerCase().includes(query);
        }).slice(0, 8);
        html = "<div class=\"shithub-comment-suggestion-section\">Suggestions</div>" + items.map(mentionHTML).join("");
      } else if (kind === "reference") {
        items = config.references.filter(function (item) {
          const needle = query.replace(/^#/, "");
          return String(item.number).includes(needle) || item.title.toLowerCase().includes(needle);
        }).slice(0, 8);
        html = "<div class=\"shithub-comment-suggestion-section\">Issues and pull requests</div>" + items.map(referenceHTML).join("");
      } else {
        items = slashCommands.filter(function (item) {
          return item.label.toLowerCase().includes(query);
        });
        html = "<div class=\"shithub-comment-suggestion-section\">Slash commands <span class=\"shithub-pill\">Preview</span></div>" + items.map(slashHTML).join("");
      }
      if (items.length === 0) {
        hideSuggestions();
        return;
      }
      suggestions.innerHTML = html;
      suggestions.hidden = false;
      suggestions.querySelector(".shithub-comment-suggestion-item")?.classList.add("is-active");
    }

    function mentionHTML(item, index) {
      const display = item.displayName ? " <small>" + escapeHTML(item.displayName) + "</small>" : "";
      return "<button type=\"button\" class=\"shithub-comment-suggestion-item\" data-suggestion-kind=\"mention\" data-suggestion-index=\"" + index + "\"><img src=\"" + escapeAttr(item.avatarUrl || "") + "\" alt=\"\"><span><strong>" + escapeHTML(item.username) + "</strong>" + display + "</span></button>";
    }

    function referenceHTML(item, index) {
      return "<button type=\"button\" class=\"shithub-comment-suggestion-item\" data-suggestion-kind=\"reference\" data-suggestion-index=\"" + index + "\"><span aria-hidden=\"true\">#</span><span class=\"shithub-comment-suggestion-copy\"><strong>#" + item.number + " " + escapeHTML(item.title) + "</strong><small>" + escapeHTML(item.kind) + " " + escapeHTML(item.state) + "</small></span></button>";
    }

    function slashHTML(item, index) {
      return "<button type=\"button\" class=\"shithub-comment-suggestion-item\" data-suggestion-kind=\"slash\" data-suggestion-index=\"" + index + "\"><span aria-hidden=\"true\">" + (item.action === "saved" ? "@" : "/") + "</span><span class=\"shithub-comment-suggestion-copy\"><strong>" + escapeHTML(item.label) + "</strong><small>" + escapeHTML(item.description) + "</small></span></button>";
    }

    function currentSuggestionItems(kind) {
      const token = activeToken || { query: "" };
      if (kind === "mention") {
        return config.mentions.filter(function (item) {
          return item.username.toLowerCase().includes(token.query) || (item.displayName || "").toLowerCase().includes(token.query);
        }).slice(0, 8);
      }
      if (kind === "reference") {
        return config.references.filter(function (item) {
          const needle = token.query.replace(/^#/, "");
          return String(item.number).includes(needle) || item.title.toLowerCase().includes(needle);
        }).slice(0, 8);
      }
      return slashCommands.filter(function (item) {
        return item.label.toLowerCase().includes(token.query);
      });
    }

    function chooseSuggestion(button) {
      const kind = button.dataset.suggestionKind;
      const index = Number(button.dataset.suggestionIndex || "0");
      const item = currentSuggestionItems(kind)[index];
      if (!item) return;
      if (kind === "mention") {
        replaceToken("@" + item.username + " ");
      } else if (kind === "reference") {
        replaceToken("#" + item.number + " ");
      } else if (item.action === "saved") {
        hideSuggestions();
        openSavedDialog();
      } else {
        replaceToken(item.snippet || "");
      }
    }

    function replaceToken(text) {
      const token = activeToken || { start: textarea.selectionStart, end: textarea.selectionEnd };
      textarea.setRangeText(text, token.start, token.end, "end");
      afterEdit();
    }

    function hideSuggestions() {
      if (!suggestions) return;
      suggestions.hidden = true;
      suggestions.innerHTML = "";
      activeToken = null;
    }

    function moveActiveSuggestion(delta) {
      if (!suggestions || suggestions.hidden) return;
      const items = Array.from(suggestions.querySelectorAll(".shithub-comment-suggestion-item"));
      if (items.length === 0) return;
      let index = items.findIndex(function (item) { return item.classList.contains("is-active"); });
      index = index < 0 ? 0 : index + delta;
      if (index < 0) index = items.length - 1;
      if (index >= items.length) index = 0;
      items.forEach(function (item, i) {
        item.classList.toggle("is-active", i === index);
      });
      items[index].scrollIntoView({ block: "nearest" });
    }

    function openSavedDialog() {
      if (!savedDialog) return;
      try {
        if (savedDialog.showModal) {
          savedDialog.showModal();
        } else {
          savedDialog.setAttribute("open", "");
        }
      } catch (error) {
        savedDialog.setAttribute("open", "");
      }
      savedDialog.querySelector("[data-comment-saved-filter]")?.focus();
    }

    function closeSavedDialog() {
      if (!savedDialog) return;
      if (savedDialog.close) {
        savedDialog.close();
      } else {
        savedDialog.removeAttribute("open");
      }
      textarea.focus();
    }

    function updateFiles(files) {
      if (!fileList || !files || files.length === 0) return;
      const names = Array.from(files).map(function (file) { return file.name; }).slice(0, 4);
      fileList.textContent = names.join(", ");
      fileList.hidden = false;
    }

    root.addEventListener("click", function (event) {
      const tab = event.target.closest("[data-comment-tab]");
      if (tab) {
        setTab(tab.dataset.commentTab);
        return;
      }
      const action = event.target.closest("[data-comment-action]");
      if (action) {
        runAction(action.dataset.commentAction);
        return;
      }
      if (event.target.closest("[data-comment-saved-replies-open]")) {
        openSavedDialog();
        return;
      }
      const suggestion = event.target.closest(".shithub-comment-suggestion-item");
      if (suggestion) {
        chooseSuggestion(suggestion);
      }
    });

    if (suggestions) {
      suggestions.addEventListener("mousedown", function (event) {
        event.preventDefault();
      });
    }

    textarea.addEventListener("input", function () {
      previewDirty = true;
      updateSubmit();
      showDetectedSuggestions();
    });
    textarea.addEventListener("keyup", function (event) {
      if (["ArrowUp", "ArrowDown", "Enter", "Escape"].includes(event.key)) return;
      showDetectedSuggestions();
    });
    textarea.addEventListener("keydown", function (event) {
      if (!suggestions || suggestions.hidden) return;
      if (event.key === "Escape") {
        event.preventDefault();
        hideSuggestions();
      } else if (event.key === "ArrowDown") {
        event.preventDefault();
        moveActiveSuggestion(1);
      } else if (event.key === "ArrowUp") {
        event.preventDefault();
        moveActiveSuggestion(-1);
      } else if (event.key === "Enter") {
        const active = suggestions.querySelector(".shithub-comment-suggestion-item.is-active");
        if (active) {
          event.preventDefault();
          chooseSuggestion(active);
        }
      }
    });
    textarea.addEventListener("blur", function () {
      window.setTimeout(hideSuggestions, 120);
    });

    if (fileInput) {
      fileInput.addEventListener("change", function () {
        updateFiles(fileInput.files);
      });
    }
    box.addEventListener("dragover", function (event) {
      if (event.dataTransfer && event.dataTransfer.files.length > 0) event.preventDefault();
    });
    box.addEventListener("drop", function (event) {
      if (event.dataTransfer && event.dataTransfer.files.length > 0) {
        event.preventDefault();
        updateFiles(event.dataTransfer.files);
      }
    });

    if (savedDialog) {
      savedDialog.addEventListener("click", function (event) {
        if (event.target.closest("[data-comment-saved-close]")) {
          closeSavedDialog();
          return;
        }
        const insert = event.target.closest("[data-comment-saved-insert]");
        if (insert) {
          closeSavedDialog();
          insertText(insert.dataset.commentSavedInsert || "");
          return;
        }
        if (event.target.closest(".shithub-comment-saved-create")) {
          closeSavedDialog();
        }
      });
      savedDialog.querySelector("[data-comment-saved-filter]")?.addEventListener("input", function (event) {
        const query = event.target.value.trim().toLowerCase();
        savedDialog.querySelectorAll("[data-comment-saved-insert]").forEach(function (button) {
          button.hidden = query !== "" && !button.textContent.toLowerCase().includes(query);
        });
      });
    }

    document.addEventListener("click", function (event) {
      if (!root.contains(event.target)) hideSuggestions();
    });

    updateSubmit();
  }

  function readConfig(root) {
    const fallback = { mentions: [], references: [] };
    const script = root.querySelector("[data-comment-editor-config]");
    if (!script) return fallback;
    try {
      const parsed = JSON.parse(script.textContent || "{}");
      return {
        mentions: Array.isArray(parsed.mentions) ? parsed.mentions.filter(function (item) {
          return item && item.username && item.username.toLowerCase() !== "copilot";
        }) : [],
        references: Array.isArray(parsed.references) ? parsed.references : []
      };
    } catch (error) {
      return fallback;
    }
  }

  function escapeHTML(value) {
    return String(value || "")
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;")
      .replace(/'/g, "&#39;");
  }

  function escapeAttr(value) {
    return escapeHTML(value).replace(/`/g, "&#96;");
  }

  document.querySelectorAll("[data-comment-editor]").forEach(initEditor);
})();
