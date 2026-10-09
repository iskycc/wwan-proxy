// Paste this entire file into the affected browser's Console, then click a Select.
// Reads DOM state only. Does not send requests, change styles, or read form values.
(() => {
  window.__wwanSelectDiagnostics?.stop();
  const selectors = ".ant-select-dropdown, .ant-dropdown";
  const events = [];
  const snapshots = [];
  const errors = [];
  const timers = [];
  const listeners = [];
  const started = performance.now();
  let selected = null;
  let captured = false;
  let stopped = false;
  const elapsed = () => Math.round(performance.now() - started);
  const viewState = () => ({
    viewport: { width: innerWidth, height: innerHeight, devicePixelRatio },
    visualViewport: window.visualViewport && {
      scale: visualViewport.scale,
      width: visualViewport.width,
      height: visualViewport.height,
    },
    media: Object.fromEntries(
      [
        "prefers-reduced-motion: reduce",
        "forced-colors: active",
        "prefers-color-scheme: dark",
      ].map((query) => [query, matchMedia("(" + query + ")").matches]),
    ),
  });
  const describe = (element) =>
    element instanceof Element
      ? {
          tag: element.tagName,
          class: element.getAttribute("class"),
          role: element.getAttribute("role"),
        }
      : null;
  const measure = (element) => {
    if (!element) return null;
    const rect = element.getBoundingClientRect();
    const style = getComputedStyle(element);
    const inViewport =
      rect.width > 0 &&
      rect.height > 0 &&
      rect.right > 0 &&
      rect.bottom > 0 &&
      rect.left < innerWidth &&
      rect.top < innerHeight;
    let hit = null;
    if (inViewport) {
      const x = (Math.max(0, rect.left) + Math.min(innerWidth, rect.right)) / 2;
      const y =
        (Math.max(0, rect.top) + Math.min(innerHeight, rect.bottom)) / 2;
      hit = document.elementFromPoint(x, y);
    }
    return {
      ...describe(element),
      connected: element.isConnected,
      rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height },
      offset: { width: element.offsetWidth, height: element.offsetHeight },
      inViewport,
      hit: describe(hit),
      hitInside: !!hit && element.contains(hit),
      inlineStyle: element.getAttribute("style"),
      computed: Object.fromEntries(
        [
          "display",
          "visibility",
          "opacity",
          "pointerEvents",
          "position",
          "zIndex",
          "left",
          "top",
          "width",
          "height",
          "overflow",
          "transform",
          "scale",
          "zoom",
          "color",
          "backgroundColor",
          "filter",
          "clipPath",
          "animationName",
          "animationDuration",
          "animationPlayState",
          "transitionDuration",
        ].map((key) => [key, style[key]]),
      ),
      animations: element.getAnimations().map((animation) => ({
        name: animation.animationName || null,
        transitionProperty: animation.transitionProperty || null,
        playState: animation.playState,
        currentTime: animation.currentTime,
      })),
    };
  };
  const snapshot = () => {
    const input = selected?.querySelector('[role="combobox"]');
    return {
      ms: elapsed(),
      ...viewState(),
      expanded: input?.getAttribute("aria-expanded"),
      disabled:
        input?.disabled || selected?.classList.contains("ant-select-disabled"),
      focused: describe(document.activeElement),
      trigger: measure(selected),
      popups: [...document.querySelectorAll(selectors)].map(measure),
    };
  };
  const environment = () => ({
    userAgent: navigator.userAgent,
    ...viewState(),
    assets: [
      ...document.querySelectorAll('script[src], link[rel="stylesheet"]'),
    ].map((element) =>
      new URL(element.src || element.href, location.href).pathname
        .split("/")
        .pop(),
    ),
  });
  const report = () => ({
    environment: environment(),
    events,
    snapshots,
    errors,
  });
  const stop = () => {
    if (stopped) return;
    stopped = true;
    for (const [type, handler] of listeners)
      window.removeEventListener(type, handler, true);
    for (const timer of timers) clearTimeout(timer);
    window.removeEventListener("error", onError);
    window.removeEventListener("unhandledrejection", onRejection);
  };
  const onError = (event) =>
    errors.push({ ms: elapsed(), message: event.message });
  const onRejection = (event) =>
    errors.push({
      ms: elapsed(),
      message: String(event.reason?.message || event.reason),
    });
  const onInteraction = (event) => {
    let trigger =
      event.target instanceof Element
        ? event.target.closest(".ant-select")
        : null;
    if (!trigger && event instanceof MouseEvent) {
      trigger = [...document.querySelectorAll(".ant-select")].find(
        (element) => {
          const rect = element.getBoundingClientRect();
          return (
            rect.width > 0 &&
            rect.height > 0 &&
            event.clientX >= rect.left &&
            event.clientX <= rect.right &&
            event.clientY >= rect.top &&
            event.clientY <= rect.bottom
          );
        },
      );
    }
    if (!trigger || (selected && trigger !== selected) || events.length >= 40)
      return;
    if (
      event instanceof KeyboardEvent &&
      !["Enter", " ", "ArrowDown", "Escape"].includes(event.key)
    )
      return;
    // Focus alone does not open a menu. Wait for the opening interaction so
    // tabbing through controls cannot consume the two-second capture window.
    if (
      !captured &&
      !["pointerdown", "mousedown", "click"].includes(event.type) &&
      !(
        event.type === "keydown" &&
        ["Enter", " ", "ArrowDown"].includes(event.key)
      )
    )
      return;
    selected = trigger;
    const entry = {
      ms: elapsed(),
      type: event.type,
      target: describe(event.target),
      trusted: event.isTrusted,
      pointerType: event.pointerType,
      key: event.key,
      expanded: trigger
        .querySelector('[role="combobox"]')
        ?.getAttribute("aria-expanded"),
      defaultPrevented: event.defaultPrevented,
    };
    events.push(entry);
    queueMicrotask(() => {
      entry.defaultPrevented = event.defaultPrevented;
    });
    if (captured) return;
    captured = true;
    snapshots.push(snapshot());
    for (const delay of [0, 100, 400, 1200])
      timers.push(setTimeout(() => snapshots.push(snapshot()), delay));
    timers.push(
      setTimeout(() => {
        console.log(
          "WWAN_SELECT_DIAGNOSTIC\n" + JSON.stringify(report(), null, 2),
        );
        stop();
      }, 2000),
    );
  };
  for (const type of [
    "pointerdown",
    "mousedown",
    "pointerup",
    "mouseup",
    "click",
    "keydown",
    "focusin",
    "focusout",
  ]) {
    listeners.push([type, onInteraction]);
    window.addEventListener(type, onInteraction, true);
  }
  window.addEventListener("error", onError);
  window.addEventListener("unhandledrejection", onRejection);
  window.__wwanSelectDiagnostics = { report, stop };
  console.log(
    "诊断已启动：点击右上角“界面主题”下拉框，等待两秒，然后复制 WWAN_SELECT_DIAGNOSTIC 输出。",
  );
})();
