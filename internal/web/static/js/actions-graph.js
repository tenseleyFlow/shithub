// SPDX-License-Identifier: AGPL-3.0-or-later

(function () {
  "use strict";

  var MIN_SCALE = 0.58;
  var MAX_SCALE = 1.75;

  function clamp(value, min, max) {
    return Math.min(max, Math.max(min, value));
  }

  function clear(element) {
    while (element.firstChild) {
      element.removeChild(element.firstChild);
    }
  }

  function appendText(parent, tagName, className, text) {
    var element = document.createElement(tagName);
    if (className) element.className = className;
    element.textContent = text;
    parent.appendChild(element);
    return element;
  }

  function parseGraph(shell) {
    var script = shell.querySelector("[data-actions-graph]");
    if (!script) return null;
    try {
      return JSON.parse(script.textContent || "{}");
    } catch (err) {
      return null;
    }
  }

  function initGraph(shell) {
    var graph = parseGraph(shell);
    var viewport = shell.querySelector("[data-actions-graph-viewport]");
    var canvas = shell.querySelector("[data-actions-graph-canvas]");
    var popover = shell.querySelector("[data-actions-graph-popover]");
    var popoverHandle = shell.querySelector("[data-actions-graph-popover-handle]");
    var popoverTitle = shell.querySelector("[data-actions-graph-popover-title]");
    var popoverState = shell.querySelector("[data-actions-graph-popover-state]");
    var popoverBody = shell.querySelector("[data-actions-graph-popover-body]");
    var closePopoverButton = shell.querySelector("[data-actions-graph-popover-close]");
    var fitButton = shell.querySelector("[data-actions-graph-fit]");
    var zoomInButton = shell.querySelector("[data-actions-graph-zoom-in]");
    var zoomOutButton = shell.querySelector("[data-actions-graph-zoom-out]");
    var resetButton = shell.querySelector("[data-actions-graph-reset]");
    var focusButton = shell.querySelector("[data-actions-graph-focus]");
    if (!graph || !viewport || !canvas || !popover) return;

    var nodesByID = {};
    (graph.nodes || []).forEach(function (node) {
      nodesByID[node.id] = node;
    });

    var scale = 1;
    var translateX = 0;
    var translateY = 0;
    var selectedNode = null;
    var selectedElement = null;
    var popoverDragged = false;

    function setTransform(nextScale, nextX, nextY) {
      scale = clamp(nextScale, MIN_SCALE, MAX_SCALE);
      translateX = nextX;
      translateY = nextY;
      canvas.style.transform = "translate(" + translateX + "px, " + translateY + "px) scale(" + scale + ")";
    }

    function fitGraph() {
      if (!graph.canvasWidth || !graph.canvasHeight) {
        setTransform(1, 0, 0);
        return;
      }
      var width = Math.max(1, viewport.clientWidth - 48);
      var nextScale = Math.min(1, width / graph.canvasWidth);
      nextScale = clamp(nextScale, MIN_SCALE, MAX_SCALE);
      setTransform(
        nextScale,
        Math.round((viewport.clientWidth - graph.canvasWidth * nextScale) / 2),
        Math.round((viewport.clientHeight - graph.canvasHeight * nextScale) / 2)
      );
    }

    function zoomAround(clientX, clientY, delta) {
      var rect = viewport.getBoundingClientRect();
      var localX = clientX - rect.left;
      var localY = clientY - rect.top;
      var graphX = (localX - translateX) / scale;
      var graphY = (localY - translateY) / scale;
      var nextScale = clamp(scale * delta, MIN_SCALE, MAX_SCALE);
      setTransform(nextScale, localX - graphX * nextScale, localY - graphY * nextScale);
    }

    function closePopover() {
      popover.hidden = true;
      selectedNode = null;
      if (selectedElement) selectedElement.classList.remove("is-selected");
      selectedElement = null;
    }

    function positionPopover(nodeElement) {
      if (popoverDragged || !nodeElement) return;
      popover.hidden = false;
      var shellRect = shell.getBoundingClientRect();
      var nodeRect = nodeElement.getBoundingClientRect();
      var popoverWidth = popover.offsetWidth || 352;
      var left = nodeRect.right - shellRect.left + 14;
      if (left + popoverWidth + 16 > shell.clientWidth) {
        left = nodeRect.left - shellRect.left - popoverWidth - 14;
      }
      left = clamp(left, 16, Math.max(16, shell.clientWidth - popoverWidth - 16));
      var top = clamp(nodeRect.top - shellRect.top, 58, Math.max(58, shell.clientHeight - 120));
      popover.style.right = "auto";
      popover.style.left = Math.round(left) + "px";
      popover.style.top = Math.round(top) + "px";
    }

    function buildStepList(node) {
      if (!node.steps || node.steps.length === 0) {
        appendText(popoverBody, "p", "shithub-actions-graph-empty-steps", "No steps were created for this job.");
        return;
      }
      var list = document.createElement("ul");
      node.steps.forEach(function (step) {
        var item = document.createElement("li");
        var status = document.createElement("span");
        status.className = "shithub-actions-state shithub-actions-state-" + step.stateClass;
        status.textContent = "●";
        status.setAttribute("aria-label", step.stateText);

        var link = document.createElement("a");
        link.href = step.logHref || ("#" + node.anchor);
        link.textContent = step.name;
        if (step.detail) {
          link.title = step.kind + " · " + step.detail;
        } else {
          link.title = step.kind;
        }

        var duration = document.createElement("small");
        duration.textContent = step.duration || "";

        item.appendChild(status);
        item.appendChild(link);
        item.appendChild(duration);
        list.appendChild(item);
      });
      popoverBody.appendChild(list);
    }

    function showPopover(node, nodeElement) {
      if (selectedElement) selectedElement.classList.remove("is-selected");
      selectedNode = node;
      selectedElement = nodeElement;
      if (selectedElement) selectedElement.classList.add("is-selected");

      popoverTitle.textContent = node.name;
      popoverState.className = "shithub-actions-state shithub-actions-state-" + node.stateClass;
      popoverState.textContent = "●";
      popoverState.setAttribute("aria-label", node.stateText);

      clear(popoverBody);
      var meta = document.createElement("div");
      meta.className = "shithub-actions-graph-popover-meta";
      appendText(meta, "span", "", node.stateText + " · " + (node.duration || "0s"));
      if (node.runsOn) appendText(meta, "span", "", "runs-on " + node.runsOn);
      if (node.needsText) appendText(meta, "span", "", "needs " + node.needsText);
      appendText(meta, "span", "", node.completedStepCount + " of " + node.stepCount + " steps complete" + (node.failureCount ? " · " + node.failureCount + " failed" : ""));
      popoverBody.appendChild(meta);
      buildStepList(node);
      popoverDragged = false;
      positionPopover(nodeElement);
    }

    shell.querySelectorAll("[data-actions-graph-node]").forEach(function (button) {
      button.addEventListener("click", function () {
        var node = nodesByID[button.getAttribute("data-job-id")];
        if (!node) return;
        showPopover(node, button);
      });
    });

    var isPanning = false;
    var panStartX = 0;
    var panStartY = 0;
    var panOriginX = 0;
    var panOriginY = 0;

    viewport.addEventListener("pointerdown", function (event) {
      if (event.button !== 0 || event.target.closest("button, a, [data-actions-graph-popover]")) return;
      isPanning = true;
      panStartX = event.clientX;
      panStartY = event.clientY;
      panOriginX = translateX;
      panOriginY = translateY;
      viewport.classList.add("is-panning");
      viewport.setPointerCapture(event.pointerId);
    });

    viewport.addEventListener("pointermove", function (event) {
      if (!isPanning) return;
      setTransform(scale, panOriginX + event.clientX - panStartX, panOriginY + event.clientY - panStartY);
      positionPopover(selectedElement);
    });

    viewport.addEventListener("pointerup", function (event) {
      if (!isPanning) return;
      isPanning = false;
      viewport.classList.remove("is-panning");
      viewport.releasePointerCapture(event.pointerId);
    });

    viewport.addEventListener("wheel", function (event) {
      event.preventDefault();
      zoomAround(event.clientX, event.clientY, event.deltaY < 0 ? 1.08 : 0.92);
      positionPopover(selectedElement);
    }, { passive: false });

    viewport.addEventListener("keydown", function (event) {
      if (event.key === "Escape") {
        if (!popover.hidden) {
          closePopover();
          event.preventDefault();
        } else if (shell.classList.contains("is-focused")) {
          toggleFocus(false);
          event.preventDefault();
        }
      } else if (event.key === "+" || event.key === "=") {
        zoomAround(viewport.getBoundingClientRect().left + viewport.clientWidth / 2, viewport.getBoundingClientRect().top + viewport.clientHeight / 2, 1.12);
        event.preventDefault();
      } else if (event.key === "-") {
        zoomAround(viewport.getBoundingClientRect().left + viewport.clientWidth / 2, viewport.getBoundingClientRect().top + viewport.clientHeight / 2, 0.88);
        event.preventDefault();
      } else if (event.key === "0") {
        fitGraph();
        positionPopover(selectedElement);
        event.preventDefault();
      }
    });

    if (closePopoverButton) {
      closePopoverButton.addEventListener("click", closePopover);
    }
    if (fitButton) {
      fitButton.addEventListener("click", function () {
        fitGraph();
        positionPopover(selectedElement);
      });
    }
    if (zoomInButton) {
      zoomInButton.addEventListener("click", function () {
        zoomAround(viewport.getBoundingClientRect().left + viewport.clientWidth / 2, viewport.getBoundingClientRect().top + viewport.clientHeight / 2, 1.12);
        positionPopover(selectedElement);
      });
    }
    if (zoomOutButton) {
      zoomOutButton.addEventListener("click", function () {
        zoomAround(viewport.getBoundingClientRect().left + viewport.clientWidth / 2, viewport.getBoundingClientRect().top + viewport.clientHeight / 2, 0.88);
        positionPopover(selectedElement);
      });
    }
    if (resetButton) {
      resetButton.addEventListener("click", function () {
        setTransform(1, 0, 0);
        positionPopover(selectedElement);
      });
    }

    function toggleFocus(force) {
      var focused = typeof force === "boolean" ? force : !shell.classList.contains("is-focused");
      shell.classList.toggle("is-focused", focused);
      document.body.classList.toggle("shithub-actions-graph-body-locked", focused);
      if (focusButton) focusButton.setAttribute("aria-pressed", focused ? "true" : "false");
      window.setTimeout(function () {
        fitGraph();
        positionPopover(selectedElement);
      }, 0);
    }
    if (focusButton) {
      focusButton.addEventListener("click", function () { toggleFocus(); });
    }

    if (popoverHandle) {
      var draggingPopover = false;
      var dragStartX = 0;
      var dragStartY = 0;
      var dragOriginLeft = 0;
      var dragOriginTop = 0;
      popoverHandle.addEventListener("pointerdown", function (event) {
        if (event.target.closest("button, a")) return;
        draggingPopover = true;
        popoverDragged = true;
        dragStartX = event.clientX;
        dragStartY = event.clientY;
        dragOriginLeft = popover.offsetLeft;
        dragOriginTop = popover.offsetTop;
        popoverHandle.setPointerCapture(event.pointerId);
      });
      popoverHandle.addEventListener("pointermove", function (event) {
        if (!draggingPopover) return;
        var maxLeft = Math.max(16, shell.clientWidth - popover.offsetWidth - 16);
        var maxTop = Math.max(58, shell.clientHeight - popover.offsetHeight - 16);
        popover.style.right = "auto";
        popover.style.left = Math.round(clamp(dragOriginLeft + event.clientX - dragStartX, 16, maxLeft)) + "px";
        popover.style.top = Math.round(clamp(dragOriginTop + event.clientY - dragStartY, 58, maxTop)) + "px";
      });
      popoverHandle.addEventListener("pointerup", function (event) {
        if (!draggingPopover) return;
        draggingPopover = false;
        popoverHandle.releasePointerCapture(event.pointerId);
      });
    }

    window.addEventListener("resize", function () {
      fitGraph();
      positionPopover(selectedElement);
    });
    fitGraph();
  }

  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll("[data-actions-graph-shell]").forEach(initGraph);
  });
})();
