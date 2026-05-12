// SPDX-License-Identifier: AGPL-3.0-or-later

(function () {
  function initMergeBox(root) {
    const openButton = root.querySelector("[data-pull-merge-open]");
    const cancelButton = root.querySelector("[data-pull-merge-cancel]");
    const choice = root.querySelector("[data-pull-merge-choice]");
    const confirm = root.querySelector("[data-pull-merge-confirm]");
    const method = root.querySelector("[data-pull-merge-method]");
    const confirmMethod = root.querySelector("[data-pull-merge-confirm-method]");
    const subject = root.querySelector("[data-pull-merge-subject]");
    if (!openButton || !choice || !confirm) return;

    function syncMethod() {
      if (method && confirmMethod) confirmMethod.value = method.value;
    }

    openButton.addEventListener("click", function () {
      syncMethod();
      choice.hidden = true;
      confirm.hidden = false;
      if (subject) subject.focus();
    });

    if (cancelButton) {
      cancelButton.addEventListener("click", function () {
        confirm.hidden = true;
        choice.hidden = false;
        openButton.focus();
      });
    }

    if (method) method.addEventListener("change", syncMethod);
  }

  document.querySelectorAll("[data-pull-merge-box]").forEach(initMergeBox);
})();
