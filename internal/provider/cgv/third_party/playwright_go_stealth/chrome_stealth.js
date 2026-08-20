// Chrome Stealth Patch - passive Chrome surface normalization only.
// Based on https://github.com/jonfriesen/playwright-go-stealth/issues/2
(() => {
  const windowsPatch = (w) => {
    const chrome = w.chrome || {};
    if (!chrome.app) {
      chrome.app = {
        isInstalled: false,
        InstallState: {
          DISABLED: "disabled",
          INSTALLED: "installed",
          NOT_INSTALLED: "not_installed",
        },
        RunningState: {
          CANNOT_RUN: "cannot_run",
          READY_TO_RUN: "ready_to_run",
          RUNNING: "running",
        },
      };
    }
    if (typeof chrome.loadTimes !== "function") chrome.loadTimes = () => {};
    if (typeof chrome.csi !== "function") chrome.csi = () => {};
    if (!w.chrome) {
      Object.defineProperty(w, "chrome", {
        value: chrome,
        configurable: true,
      });
    }
    w.navigator.permissions.query = new Proxy(navigator.permissions.query, {
      apply: async function (target, thisArg, args) {
        try {
          const result = await Reflect.apply(target, thisArg, args);
          if (result?.state === "prompt") {
            Object.defineProperty(result, "state", { value: "denied" });
          }
          return Promise.resolve(result);
        } catch (error) {
          return Promise.reject(error);
        }
      },
    });
  };
  windowsPatch(window);
})();
