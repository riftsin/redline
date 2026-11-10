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
      case "flash":
        flash(payload.selectors);
        break;
      case "diff":
        diffAndReport(payload);
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
      ".change{position:fixed;box-sizing:border-box;border:2px solid " + ACCENT + ";border-radius:4px;" +
      "background:rgba(" + ACCENT_RGB + ",.14);animation:change 2.6s ease-out forwards}" +
      "@keyframes change{0%,55%{opacity:1}100%{opacity:0}}" +
      "</style>" +
      '<div class="box hover"></div><div class="box selected"></div><div class="texts"></div><div class="flashes"></div>';
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

  // Briefly outlines every given block, scrolling the first into view when
  // none of them is visible.
  function flash(selectors) {
    const holder = overlay().querySelector(".flashes");
    holder.textContent = "";
    const els = [];
    for (const sel of selectors || []) {
      let el = null;
      try {
        el = typeof sel === "string" ? document.querySelector(sel) : null;
      } catch (_error) {
        el = null;
      }
      if (el) els.push(el);
    }
    if (els.length === 0) return;
    const visible = (el) => {
      const r = el.getBoundingClientRect();
      return r.bottom > 0 && r.top < document.documentElement.clientHeight;
    };
    const draw = () => {
      holder.textContent = "";
      for (const el of els) {
        const r = el.getBoundingClientRect();
        if (r.width <= 0 || r.height <= 0) continue;
        const box = document.createElement("div");
        box.className = "change";
        setBox(box, r, 4);
        holder.appendChild(box);
      }
      setTimeout(() => {
        holder.textContent = "";
      }, 2700);
    };
    if (!els.some(visible)) {
      els[0].scrollIntoView({ block: "center", behavior: "smooth" });
      setTimeout(draw, 400);
    } else {
      draw();
    }
  }

  // -- What changed ------------------------------------------------------------
  //
  // After the agent saves, the review page sends the source the reviewer saw
  // and the new one. Both are parsed (never run) and compared as documents,
  // so formatting - indentation, line breaks, attribute order or quoting,
  // class order, comments - never counts as a change. Only the innermost
  // changed blocks are reported and flashed; a page that changed almost
  // everywhere is reported as rewritten instead.

  const SKIPPED = { script: 1, style: 1, noscript: 1, template: 1, link: 1, meta: 1, title: 1, base: 1 };
  const INLINE = {
    a: 1, abbr: 1, b: 1, bdi: 1, bdo: 1, br: 1, cite: 1, code: 1, data: 1, dfn: 1, em: 1, i: 1, kbd: 1,
    mark: 1, q: 1, s: 1, samp: 1, small: 1, span: 1, strong: 1, sub: 1, sup: 1, time: 1, u: 1, var: 1, wbr: 1,
  };
  const REWRITE_SHARE = 0.6;

  function fnv(str) {
    let h = 0x811c9dc5;
    for (let i = 0; i < str.length; i++) {
      h ^= str.charCodeAt(i);
      h = Math.imul(h, 0x01000193);
    }
    return (h >>> 0).toString(36);
  }

  function collapse(text) {
    return text.replace(/\s+/g, " ").trim();
  }

  function shallow(el) {
    const attrs = [];
    for (const a of Array.from(el.attributes)) {
      const value = a.name === "class" ? a.value.split(/\s+/).filter(Boolean).sort().join(" ") : collapse(a.value);
      attrs.push(a.name + "=" + value);
    }
    attrs.sort();
    return el.localName + "[" + attrs.join("|") + "]";
  }

  function significant(el) {
    const out = [];
    for (const child of Array.from(el.childNodes)) {
      if (child.nodeType === 1 && !SKIPPED[child.localName]) out.push(child);
      else if (child.nodeType === 3 && collapse(child.nodeValue) !== "") out.push(child);
    }
    return out;
  }

  function hashOf(node, memo) {
    if (memo.has(node)) return memo.get(node);
    let h;
    if (node.nodeType === 3) h = "t" + fnv(collapse(node.nodeValue));
    else h = "e" + fnv(shallow(node) + "{" + significant(node).map((c) => hashOf(c, memo)).join(",") + "}");
    memo.set(node, h);
    return h;
  }

  // Longest common subsequence of two hash lists; returns matched index pairs.
  function lcs(a, b) {
    if (a.length * b.length > 250000) return null;
    const rows = a.length + 1;
    const cols = b.length + 1;
    const table = new Uint16Array(rows * cols);
    for (let i = a.length - 1; i >= 0; i--) {
      for (let j = b.length - 1; j >= 0; j--) {
        table[i * cols + j] = a[i] === b[j] ? table[(i + 1) * cols + j + 1] + 1 : Math.max(table[(i + 1) * cols + j], table[i * cols + j + 1]);
      }
    }
    const pairs = [];
    let i = 0;
    let j = 0;
    while (i < a.length && j < b.length) {
      if (a[i] === b[j]) {
        pairs.push([i, j]);
        i++;
        j++;
      } else if (table[(i + 1) * cols + j] >= table[i * cols + j + 1]) i++;
      else j++;
    }
    return pairs;
  }

  function blockOf(el) {
    while (el && el.parentElement && INLINE[el.localName]) el = el.parentElement;
    return el;
  }

  function diffChildren(oldEl, newEl, oldMemo, newMemo, changed) {
    const a = significant(oldEl);
    const b = significant(newEl);
    const pairs = lcs(a.map((n) => hashOf(n, oldMemo)), b.map((n) => hashOf(n, newMemo)));
    if (pairs === null) {
      changed.add(blockOf(newEl));
      return;
    }
    pairs.push([a.length, b.length]);
    let ai = 0;
    let bi = 0;
    for (const [ma, mb] of pairs) {
      const oldGap = a.slice(ai, ma);
      const newGap = b.slice(bi, mb);
      let textChanged = false;
      for (let k = 0; k < newGap.length; k++) {
        const n = newGap[k];
        const o = oldGap[k];
        if (n.nodeType === 3) {
          textChanged = true;
        } else if (o && o.nodeType === 1 && shallow(o) === shallow(n)) {
          diffChildren(o, n, oldMemo, newMemo, changed);
        } else {
          changed.add(blockOf(n));
        }
      }
      if (oldGap.length > newGap.length) {
        // Something was removed here: the surrounding block changed.
        const removedText = oldGap.slice(newGap.length).some((n) => n.nodeType === 3);
        if (removedText || newEl.localName !== "body") textChanged = true;
        else if (b[mb]) changed.add(blockOf(b[mb]));
      }
      if (textChanged && newEl.localName !== "body") changed.add(blockOf(newEl));
      ai = ma + 1;
      bi = mb + 1;
    }
  }

  function elementCount(el) {
    return el.getElementsByTagName("*").length + 1;
  }

  function diffAndReport(payload) {
    const revision = Number(payload.revision) || 0;
    let selectors = [];
    let rewrite = false;
    try {
      const parser = new DOMParser();
      const before = parser.parseFromString(String(payload.previous || ""), "text/html").body;
      const after = parser.parseFromString(String(payload.current || ""), "text/html").body;
      const changed = new Set();
      diffChildren(before, after, new Map(), new Map(), changed);
      // Keep the innermost blocks only.
      const list = Array.from(changed).filter((el) => el && el !== after);
      const innermost = list.filter((el) => !list.some((other) => other !== el && el.contains(other)));
      const total = elementCount(after);
      const covered = innermost.reduce((sum, el) => sum + elementCount(el), 0);
      if (total > 6 && covered / total > REWRITE_SHARE) rewrite = true;
      else selectors = innermost.map((el) => selector(el)).filter((sel) => sel && document.querySelector(sel));
    } catch (_error) {
      selectors = [];
    }
    post("changes", { revision, selectors: rewrite ? [] : selectors, rewrite });
    if (!rewrite && selectors.length) flash(selectors);
  }

  // -- Table cells -------------------------------------------------------------
  //
  // A clicked cell also carries its visible row and column names, so feedback
  // still reads right when the table is sorted or filtered. A name is given
  // only when it is provable: merged cells (rowspan/colspan) that make it
  // ambiguous leave it out rather than guessing. An explicit
  // <th scope="row"> is authoritative. (Adapted from lavish-axi.)

  function tagOf(el) {
    return String((el && (el.tagName || el.nodeName)) || "").toLowerCase();
  }

  function rowsIn(el) {
    const rows = [];
    for (const child of Array.from((el && el.children) || [])) {
      const tag = tagOf(child);
      if (tag === "td" || tag === "th" || tag === "table") continue;
      if (tag === "tr") rows.push(child);
      else rows.push(...rowsIn(child));
    }
    return rows;
  }

  function cellsOf(row) {
    return Array.from((row && row.children) || []).filter((c) => tagOf(c) === "td" || tagOf(c) === "th");
  }

  function spanValue(cell, name, parsed) {
    if (typeof parsed === "number" && Number.isInteger(parsed) && parsed >= 0) return parsed;
    const digits = /^[\t\n\f\r ]*(\d+)/.exec(String((cell && cell.getAttribute && cell.getAttribute(name)) || ""));
    return digits ? Number(digits[1]) : null;
  }

  function colSpan(cell) {
    const span = spanValue(cell, "colspan", cell && cell.colSpan);
    return span !== null && span >= 1 ? span : 1;
  }

  function spansRows(cell, distance) {
    const span = spanValue(cell, "rowspan", cell && cell.rowSpan);
    const rendered = span === null || span < 0 ? 1 : span;
    return rendered === 0 || rendered > distance;
  }

  function rowGroup(table, row) {
    let ancestor = (row && row.parentElement) || null;
    while (ancestor && ancestor !== table) {
      const tag = tagOf(ancestor);
      if (tag === "thead" || tag === "tbody" || tag === "tfoot") return ancestor;
      ancestor = ancestor.parentElement;
    }
    return table;
  }

  function rowIsShifted(table, row) {
    if (!table || !row) return true;
    const rows = rowsIn(rowGroup(table, row));
    const index = rows.indexOf(row);
    if (index < 0) return true;
    for (let i = 0; i < index; i += 1) {
      for (const cell of cellsOf(rows[i])) if (spansRows(cell, index - i)) return true;
    }
    return false;
  }

  function headerRowOf(table) {
    const head = Array.from((table && table.children) || []).find((c) => tagOf(c) === "thead");
    if (head) return rowsIn(head).slice(-1)[0] || null;
    const first = rowsIn(table)[0];
    const cells = cellsOf(first);
    return cells.length > 0 && cells.every((c) => tagOf(c) === "th") ? first : null;
  }

  function cellText(el) {
    return el ? excerpt(el) : "";
  }

  function columnLabel(headerRow, cells, index) {
    if (!headerRow) return "";
    const headers = cellsOf(headerRow);
    const headerWidth = headers.reduce((sum, c) => sum + colSpan(c), 0);
    const rowWidth = cells.reduce((sum, c) => sum + colSpan(c), 0);
    if (headerWidth === 0 || headerWidth !== rowWidth) return "";
    let start = 0;
    for (let i = 0; i < index; i += 1) start += colSpan(cells[i]);
    const end = start + colSpan(cells[index]);
    let cursor = 0;
    for (const header of headers) {
      const next = cursor + colSpan(header);
      if (cursor === start && next === end) return cellText(header);
      if (start < next) return "";
      cursor = next;
    }
    return "";
  }

  function tableCellTarget(el) {
    const cell = el.closest && el.closest("td,th");
    const row = cell && cell.closest("tr");
    const table = row && row.closest("table");
    if (!cell || !row || !table) return null;
    const cells = cellsOf(row);
    const index = cells.indexOf(cell);
    if (index < 0) return null;
    const headerRow = headerRowOf(table);
    const shifted = rowIsShifted(table, row);
    const gridShifted = shifted || (headerRow ? rowIsShifted(table, headerRow) : false);
    const declared = cells.find(
      (c) => tagOf(c) === "th" && String((c.getAttribute && c.getAttribute("scope")) || "").toLowerCase() === "row",
    );
    const allHeaders = cells.every((c) => tagOf(c) === "th");
    const inHeader = headerRow === row || !!cell.closest("thead");
    const rowHeading = inHeader ? null : declared || (allHeaders || shifted ? null : cells[0]);
    return {
      type: "table-cell",
      selector: selector(cell).slice(0, 240),
      rowLabel: cellText(rowHeading),
      columnLabel: gridShifted ? "" : columnLabel(headerRow, cells, index),
      text: cellText(cell),
    };
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
      const payload = context(el);
      const cell = tableCellTarget(el);
      if (cell && (cell.rowLabel || cell.columnLabel)) payload.target = cell;
      post("select-element", payload);
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

  // Lets a long-lived artifact keep the browser tab current. A string sets
  // the field, null restores the value the page loaded with, and an omitted
  // field is left as it is.
  function setDocumentMeta(meta) {
    const m = meta || {};
    const out = {};
    if (Object.prototype.hasOwnProperty.call(m, "title")) out.title = m.title === null ? null : String(m.title);
    if (Object.prototype.hasOwnProperty.call(m, "icon")) out.icon = m.icon === null ? null : absolute(String(m.icon));
    post("meta", out);
  }

  function absolute(href) {
    try {
      return new URL(href, location.href).href;
    } catch (_error) {
      return "";
    }
  }

  window.redline = {
    queuePrompt,
    sendQueuedPrompts: () => post("send"),
    endSession: () => post("end-session"),
    snapshot,
    setDocumentMeta,
  };

  // -- Start -----------------------------------------------------------------

  let started = false;

  function announce() {
    const icon = document.querySelector("link[rel~='icon'][href]");
    post("ready", {
      title: document.title || "",
      icon: icon ? absolute(icon.getAttribute("href")) : "",
      annotate: annotateDefault,
    });
  }

  function ready() {
    started = true;
    setMode(mode);
    announce();
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", ready, { once: true });
  else ready();
})();
