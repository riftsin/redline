// redline artifact SDK. Injected (as the one extra <script> tag) into the
// artifact while it is reviewed in redline's sandboxed iframe. It is a
// sensor: it reports what only the browser can see (the element or text the
// reviewer points at, answers the artifact itself queues, scroll position,
// local assets that failed to load) to the review page, which owns all state
// and UI.
//
// It never changes the artifact's own elements: highlights are drawn in an
// overlay inside a shadow root. Opened outside redline, it does nothing.
// Plain script, no dependencies, served as-is.
(() => {
  "use strict";

  if (window.parent === window || window.__redlineSdk) return;
  window.__redlineSdk = true;

  const ACCENT = "#d0482b";
  const ACCENT_RGB = "208,72,43";
  const EXCERPT_MAX = 240;
  const IS_MAC = /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent || "");

  // Clicks on these stay interactive in annotate mode, without markup:
  // native controls, editable regions, and custom widgets with an
  // interactive ARIA role.
  const INTERACTIVE_ROLES = [
    "button", "checkbox", "combobox", "menuitem", "menuitemcheckbox",
    "menuitemradio", "option", "radio", "switch", "tab", "treeitem",
  ];
  const PASS_THROUGH =
    "button,input,select,textarea,option,optgroup,label,summary," +
    "[contenteditable]:not([contenteditable='false']),[data-redline-action]," +
    INTERACTIVE_ROLES.map((role) => "[role='" + role + "']").join(",");

  // The artifact may declare that it starts in explore mode, so stray clicks
  // do not open notes: <html data-redline-annotate="off">.
  const annotateDefault =
    String(document.documentElement.getAttribute("data-redline-annotate") || "").trim().toLowerCase() === "off"
      ? "explore"
      : "annotate";

  let mode = annotateDefault;
  let hovered = null;
  let selectedElement = null;
  let selectedRange = null;
  let scrollTimer = 0;

  // -- Messaging -------------------------------------------------------------

  function post(type, payload) {
    window.parent.postMessage({ redline: true, type, payload: payload || {} }, "*");
  }

  window.addEventListener("message", (event) => {
    // The review page is the only legitimate sender.
    if (event.source !== window.parent) return;
    const data = event.data;
    if (!data || data.redline !== true) return;
    const payload = data.payload || {};

    switch (data.type) {
      case "hello":
        // The review page started listening after this frame loaded.
        if (started) announce();
        break;
      case "set-mode":
        setMode(payload.mode);
        break;
      case "clear-selection":
        clearSelection();
        break;
      case "request-snapshot":
        post("snapshot", { id: payload.id, snapshot: snapshot() });
        break;
      case "restore-scroll":
        window.scrollTo(0, Number(payload.y) || 0);
        break;
      case "reveal":
        reveal(payload.selector);
        break;
    }
  });

  // -- Overlay ---------------------------------------------------------------

  let overlayRoot = null;

  function overlay() {
    if (overlayRoot) return overlayRoot;
    const host = document.createElement("redline-overlay");
    host.setAttribute("data-redline-ui", "");
    host.style.cssText = "position:fixed;inset:0;pointer-events:none;z-index:2147483647;";
    overlayRoot = host.attachShadow({ mode: "open" });
    overlayRoot.innerHTML =
      "<style>" +
      ".box{position:fixed;box-sizing:border-box;border:2px solid " + ACCENT + ";border-radius:3px;display:none}" +
      ".hover{background:rgba(" + ACCENT_RGB + ",.06)}" +
      ".selected{background:rgba(" + ACCENT_RGB + ",.12);box-shadow:0 0 0 4px rgba(" + ACCENT_RGB + ",.18)}" +
      ".flash{animation:flash 1.2s ease-out}" +
      "@keyframes flash{0%{box-shadow:0 0 0 8px rgba(" + ACCENT_RGB + ",.45)}100%{box-shadow:0 0 0 0 rgba(" + ACCENT_RGB + ",0)}}" +
      ".text{position:fixed;background:rgba(" + ACCENT_RGB + ",.28);border-radius:2px}" +
      "</style>" +
      '<div class="box hover"></div><div class="box selected"></div><div class="texts"></div>';
    (document.body || document.documentElement).appendChild(host);
    return overlayRoot;
  }

  // Positions `box` around rect `r` with `pad` pixels of room, kept inside
  // the viewport so an outline never loses an edge off-screen.
  function setBox(box, r, pad) {
    const left = Math.max(1, r.left - pad);
    const top = Math.max(1, r.top - pad);
    const right = Math.min(document.documentElement.clientWidth - 1, r.right + pad);
    const bottom = Math.min(document.documentElement.clientHeight - 1, r.bottom + pad);
    box.style.left = left + "px";
    box.style.top = top + "px";
    box.style.width = Math.max(0, right - left) + "px";
    box.style.height = Math.max(0, bottom - top) + "px";
  }

  function placeBox(kind, el) {
    const box = overlay().querySelector(".box." + kind);
    if (!el || !el.isConnected) {
      box.style.display = "none";
      return box;
    }
    box.style.display = "block";
    setBox(box, el.getBoundingClientRect(), 3);
    return box;
  }

  function drawTextRange(range) {
    const holder = overlay().querySelector(".texts");
    holder.textContent = "";
    if (!range) return;
    for (const rect of range.getClientRects()) {
      if (rect.width <= 0 || rect.height <= 0) continue;
      const mark = document.createElement("div");
      mark.className = "text";
      mark.style.left = rect.left + "px";
      mark.style.top = rect.top + "px";
      mark.style.width = rect.width + "px";
      mark.style.height = rect.height + "px";
      holder.appendChild(mark);
    }
  }

  function redraw() {
    placeBox("hover", mode === "annotate" ? hovered : null);
    placeBox("selected", selectedElement);
    drawTextRange(selectedRange);
  }

  window.addEventListener("scroll", redraw, { passive: true, capture: true });
  window.addEventListener("resize", redraw, { passive: true });

  // -- Modes -----------------------------------------------------------------

  let cursorStyle = null;

  function setMode(next) {
    mode = next === "explore" ? "explore" : "annotate";
    if (mode === "annotate" && !cursorStyle) {
      cursorStyle = document.createElement("style");
      cursorStyle.setAttribute("data-redline-ui", "");
      cursorStyle.textContent =
        "*{cursor:default!important}" +
        PASS_THROUGH + ",a{cursor:pointer!important}" +
        "input:not([type=button]):not([type=submit]):not([type=reset]):not([type=checkbox]):not([type=radio])," +
        "textarea,[contenteditable]:not([contenteditable='false']){cursor:text!important}";
      (document.head || document.documentElement).appendChild(cursorStyle);
    } else if (mode === "explore" && cursorStyle) {
      cursorStyle.remove();
      cursorStyle = null;
    }
    if (mode === "explore") hovered = null;
    redraw();
  }

  // -- Targets ---------------------------------------------------------------

  function isRedlineUi(el) {
    return !!(el && el.closest && el.closest("[data-redline-ui]"));
  }

  // Native controls and interactive widgets keep working in annotate mode.
  // A link stays annotatable even inside such a widget: letting the click
  // through would navigate the artifact frame away.
  function isPassThrough(el) {
    if (!el || !el.closest) return false;
    if (el.closest("[data-redline-action]")) return true;
    if (el.closest("a[href]")) return false;
    return !!el.closest(PASS_THROUGH);
  }

  // Ctrl+click (Cmd+click on macOS) follows a link instead of annotating it,
  // opening it in a new tab as the browser normally would.
  function isFollowClick(event) {
    return (IS_MAC ? event.metaKey : event.ctrlKey) && !!(event.target.closest && event.target.closest("a[href]"));
  }

  function cssEscape(value) {
    return window.CSS && CSS.escape ? CSS.escape(value) : String(value).replace(/[^a-zA-Z0-9_-]/g, "\\$&");
  }

  // A short, stable CSS path: up to five levels, stopping at an id.
  function selector(el) {
    if (!el || !el.tagName) return "";
    const parts = [];
    let node = el;
    while (node && node.nodeType === 1 && parts.length < 5) {
      let part = node.tagName.toLowerCase();
      if (node.id) {
        parts.unshift(part + "#" + cssEscape(node.id));
        break;
      }
      const parent = node.parentElement;
      if (parent) {
        const same = Array.from(parent.children).filter((child) => child.tagName === node.tagName);
        if (same.length > 1) part += ":nth-of-type(" + (same.indexOf(node) + 1) + ")";
      }
      parts.unshift(part);
      node = parent;
    }
    return parts.join(" > ");
  }

  function excerpt(el) {
    return (el.innerText || el.textContent || "").trim().replace(/\s+/g, " ").slice(0, EXCERPT_MAX);
  }

  function context(el) {
    return { selector: selector(el), tag: (el.tagName || "").toLowerCase(), text: excerpt(el) };
  }

  function closestElement(node) {
    if (!node) return document.body;
    return node.nodeType === 1 ? node : node.parentElement || document.body;
  }

  function nodePath(node, root) {
    const path = [];
    let current = node;
    while (current && current !== root) {
      const parentNode = current.parentNode;
      if (!parentNode) break;
      path.unshift(Array.prototype.indexOf.call(parentNode.childNodes, current));
      current = parentNode;
    }
    return path;
  }

  function boundary(node, offset) {
    const el = closestElement(node);
    return { selector: selector(el), path: nodePath(node, el), offset: Number(offset) || 0 };
  }

  function textSelection() {
    const selection = window.getSelection();
    if (!selection || selection.rangeCount === 0) return null;
    const range = selection.getRangeAt(0);
    const text = selection.toString().trim().replace(/\s+/g, " ");
    if (range.collapsed || !text) return null;
    const ancestor = closestElement(range.commonAncestorContainer);
    if (isRedlineUi(ancestor) || isPassThrough(ancestor)) return null;
    const container = selector(ancestor);
    return {
      range: range.cloneRange(),
      payload: {
        selector: container,
        tag: "text",
        text: text.slice(0, EXCERPT_MAX),
        target: {
          type: "text-range",
          text,
          selector: container,
          start: boundary(range.startContainer, range.startOffset),
          end: boundary(range.endContainer, range.endOffset),
        },
      },
    };
  }

  function clearSelection() {
    selectedElement = null;
    selectedRange = null;
    redraw();
  }

  function reveal(sel) {
    let el = null;
    try {
      el = sel ? document.querySelector(sel) : null;
    } catch (_error) {
      el = null;
    }
    if (!el) return;
    el.scrollIntoView({ block: "center", behavior: "smooth" });
    setTimeout(() => {
      const box = placeBox("selected", el);
      box.classList.remove("flash");
      void box.offsetWidth;
      box.classList.add("flash");
      setTimeout(() => placeBox("selected", selectedElement), 1200);
    }, 350);
  }

  // -- Pointer ---------------------------------------------------------------

  document.addEventListener(
    "mouseover",
    (event) => {
      if (mode !== "annotate") return;
      const el = event.target;
      hovered =
        el && el.nodeType === 1 && !isRedlineUi(el) && !isPassThrough(el) && el !== document.documentElement
          ? el
          : null;
      redraw();
    },
    true,
  );

  document.addEventListener(
    "mouseout",
    (event) => {
      if (mode === "annotate" && !event.relatedTarget) {
        hovered = null;
        redraw();
      }
    },
    true,
  );

  document.addEventListener(
    "mouseup",
    () => {
      if (mode !== "annotate") return;
      // Let the browser finish updating the selection first.
      setTimeout(() => {
        const selection = textSelection();
        if (!selection) return;
        selectedElement = null;
        selectedRange = selection.range;
        redraw();
        post("select-text", selection.payload);
      }, 0);
    },
    true,
  );

  document.addEventListener(
    "click",
    (event) => {
      if (mode !== "annotate") return;
      const el = event.target;
      if (!el || el.nodeType !== 1 || isRedlineUi(el) || isPassThrough(el)) return;
      if (isFollowClick(event)) return;
      // A drag that selected text is a text annotation, not a click.
      const selection = window.getSelection();
      if (selection && !selection.isCollapsed && selection.toString().trim()) return;
      event.preventDefault();
      event.stopPropagation();
      selectedElement = el;
      selectedRange = null;
      redraw();
      post("select-element", context(el));
    },
    true,
  );

  // -- Keyboard --------------------------------------------------------------

  document.addEventListener(
    "keydown",
    (event) => {
      const key = String(event.key || "").toLowerCase();
      if ((event.metaKey || event.ctrlKey) && !event.shiftKey && !event.altKey && key === "i") {
        event.preventDefault();
        post("toggle-mode");
      }
    },
    true,
  );

  // -- Scroll ----------------------------------------------------------------

  window.addEventListener(
    "scroll",
    () => {
      clearTimeout(scrollTimer);
      scrollTimer = setTimeout(() => post("scroll", { y: window.scrollY }), 150);
    },
    { passive: true },
  );

  // -- Local asset failures --------------------------------------------------

  window.addEventListener(
    "error",
    (event) => {
      const target = event.target;
      if (!target || target === window || !target.tagName) return;
      const source = target.currentSrc || target.src || target.href;
      if (!source) return;
      let url;
      try {
        url = new URL(source, location.href);
      } catch (_error) {
        return;
      }
      // Only the artifact's own (same server) assets; remote CDNs may be offline on purpose.
      if (url.host !== location.host || !url.pathname.startsWith("/artifact/")) return;
      post("asset-failure", { url: decodeURIComponent(url.pathname.split("/").slice(3).join("/")) });
    },
    true,
  );

  // -- Snapshot --------------------------------------------------------------

  // A compact outline of the rendered page (tag and leading text per element),
  // so the agent sees what the reviewer saw, including state scripts produced.
  function snapshot() {
    const lines = [];
    function walk(el, depth) {
      if (!(el instanceof Element) || depth > 6 || isRedlineUi(el)) return;
      const tag = el.tagName.toLowerCase();
      if (tag === "script" || tag === "style") return;
      const text = excerpt(el).slice(0, 80).replace(/"/g, "'");
      lines.push("  ".repeat(depth) + tag + (el.id ? "#" + el.id : "") + (text ? ' "' + text + '"' : ""));
      for (const child of el.children) walk(child, depth + 1);
    }
    if (document.body) walk(document.body, 0);
    return lines.join("\n");
  }

  // -- Public API for artifact authors ---------------------------------------

  // Replacement key: re-answering the same question keeps one queued answer.
  function queueKeyFor(el, options) {
    if (options && Object.prototype.hasOwnProperty.call(options, "queueKey")) return String(options.queueKey || "");
    const question = el && el.closest ? el.closest("[data-redline-question]") : null;
    if (question) return "question:" + question.getAttribute("data-redline-question");
    return "";
  }

  function queuePrompt(prompt, options) {
    const opts = options || {};
    const origin = opts.element || document.activeElement || document.body;
    const item = Object.assign(context(origin), { prompt: String(prompt || ""), queueKey: queueKeyFor(origin, opts) });
    if (opts.selector) item.selector = String(opts.selector);
    if (opts.tag) item.tag = String(opts.tag);
    if (opts.text) item.text = String(opts.text);
    if (opts.target) item.target = opts.target;
    if (opts.data) item.prompt += "\n\nContext data:\n" + JSON.stringify(opts.data, null, 2);
    post("queue-prompt", item);
  }

  window.redline = {
    queuePrompt,
    sendQueuedPrompts: () => post("send"),
    endSession: () => post("end-session"),
    snapshot,
  };

  // -- Start -----------------------------------------------------------------

  let started = false;

  function announce() {
    post("ready", { title: document.title || "", annotate: annotateDefault });
  }

  function ready() {
    started = true;
    setMode(mode);
    announce();
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", ready, { once: true });
  else ready();
})();
