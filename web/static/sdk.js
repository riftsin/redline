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
        flash(payload.selectors, true);
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

  // -- Mermaid diagram nodes --------------------------------------------------
  //
  // A click inside a rendered Mermaid diagram annotates the node, anchored to
  // Mermaid's own node id and visible label rather than whichever shape was
  // under the cursor. Keyed on Mermaid's output markers, so pasted CDN
  // diagrams and other Mermaid versions match too. (Adapted from lavish-axi.)

  function isMermaidSvg(svg) {
    if (!svg) return false;
    const id = svg.id || "";
    if (id.startsWith("mermaid-") || id.startsWith("mermaid_")) return true;
    if (svg.getAttribute && svg.getAttribute("aria-roledescription")) return true;
    return !!(svg.closest && svg.closest(".mermaid,[data-redline-mermaid]"));
  }

  function mermaidNodeElement(el) {
    if (!el || !el.closest) return null;
    const node = el.closest("g.node, g.nodes > g");
    if (!node) return null;
    const svg = node.closest("svg");
    return svg && isMermaidSvg(svg) ? node : null;
  }

  function nodeLabel(labelEl) {
    if (!labelEl) return "";
    let source = labelEl;
    if (labelEl.querySelector && labelEl.querySelector("br")) {
      source = labelEl.cloneNode(true);
      for (const br of source.querySelectorAll("br")) br.replaceWith(document.createTextNode(" "));
    }
    return (source.textContent || "").trim().replace(/\s+/g, " ").slice(0, 120);
  }

  function mermaidNodeTarget(el) {
    const node = mermaidNodeElement(el);
    if (!node) return null;
    const svg = node.closest("svg");
    return {
      type: "mermaid-node",
      diagramId: svg.id || "",
      nodeId: node.id || "",
      label: nodeLabel(node.querySelector(".nodeLabel, .label, foreignObject span, text")),
      selector: selector(node),
    };
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

  // Briefly outlines every given block. Asked for by the reviewer, it
  // scrolls the first into view when none is visible; the automatic flash
  // after a save never moves the reviewer's place.
  function flash(selectors, scroll) {
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
    if (!els.some(visible) && scroll) {
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
    if (!rewrite && selectors.length) flash(selectors, false);
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
          ? mermaidNodeElement(el) || el
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
      const node = mermaidNodeTarget(el);
      selectedElement = node ? mermaidNodeElement(el) : el;
      selectedRange = null;
      redraw();
      const payload = context(selectedElement);
      const cell = tableCellTarget(el);
      if (node) {
        payload.tag = "mermaid-node";
        payload.text = node.label || payload.text;
        payload.selector = node.selector;
        payload.target = node;
      } else if (cell && (cell.rowLabel || cell.columnLabel)) {
        payload.target = cell;
      }
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

  // -- Layout audit ------------------------------------------------------------
  //
  // A passive check for severe layout failures, proven from rendered
  // evidence: content that materially escapes the page, text clipped by its
  // own container, controls cut off or out of reach, and text almost
  // entirely covered by another element. Explicit ellipsis and line clamp,
  // visually hidden accessibility text, intentional scrollers, masks,
  // diagrams and anything moving are left alone. A finding must show up in
  // two samples taken after fonts, layout and finite animations settle. The
  // result only feeds the reviewer's inbox; it never wakes the agent.
  // (Adapted from lavish-axi.)

  const AUDIT_SETTLE_MS = 180;
  const AUDIT_MAX_WAIT_MS = 2000;
  const AUDIT_ANIMATION_MAX_WAIT_MS = 4000;
  const AUDIT_STABLE_SAMPLE_MS = 120;
  let auditTimer = 0;
  let auditRun = 0;
  let lastAuditSignature = null;

  function px(value) {
    const parsed = Number.parseFloat(String(value || "0"));
    return Number.isFinite(parsed) ? parsed : 0;
  }

  function elementText(el) {
    return String((el && (el.innerText || el.textContent)) || "").trim().replace(/\s+/g, " ");
  }

  function directText(el) {
    return Array.from((el && el.childNodes) || [])
      .filter((node) => node.nodeType === 3)
      .map((node) => String(node.textContent || ""))
      .join(" ")
      .trim()
      .replace(/\s+/g, " ");
  }

  function isRequiredControl(el) {
    if (!el || !el.matches || !el.matches("button,input,select,textarea,a[href],summary,[data-redline-action],[role]")) return false;
    if (el.matches("input[type='hidden'],[disabled],[aria-disabled='true']")) return false;
    if (!el.hasAttribute("role")) return true;
    return ["button", "link", "checkbox", "radio", "switch", "textbox", "combobox"].includes(String(el.getAttribute("role") || "").toLowerCase());
  }

  function isTextBoundary(el) {
    return !!(
      el &&
      el.matches &&
      el.matches("p,h1,h2,h3,h4,h5,h6,button,label,a[href],li,dt,dd,th,td,legend,figcaption,summary,[role='button'],[role='link'],[role='alert'],[role='status']")
    );
  }

  function hasTextBoundaryAncestor(el) {
    let node = el && el.parentElement;
    while (node && node !== document.body && node !== document.documentElement) {
      if (isTextBoundary(node)) return true;
      node = node.parentElement;
    }
    return false;
  }

  function auditedText(el) {
    return isTextBoundary(el) ? elementText(el) : directText(el);
  }

  function isVisibleForAudit(el, rect) {
    const r = rect || el.getBoundingClientRect();
    if (!el || isRedlineUi(el) || r.width <= 0 || r.height <= 0) return false;
    for (let node = el; node && node.nodeType === 1; node = node.parentElement) {
      const style = getComputedStyle(node);
      const opacity = Number.parseFloat(style.opacity || "1");
      if (style.display === "none" || style.visibility === "hidden" || style.contentVisibility === "hidden" || (Number.isFinite(opacity) && opacity <= 0.01)) {
        return false;
      }
    }
    return true;
  }

  function scrollsX(el) {
    if (!el || el === document.body || el === document.documentElement) return false;
    const v = getComputedStyle(el).overflowX;
    return v === "auto" || v === "scroll";
  }

  function scrollsY(el) {
    if (!el || el === document.body || el === document.documentElement) return false;
    const v = getComputedStyle(el).overflowY;
    return v === "auto" || v === "scroll";
  }

  function hasHorizontalScrollerAncestor(el) {
    for (let node = el; node && node.nodeType === 1 && node !== document.body && node !== document.documentElement; node = node.parentElement) {
      if (scrollsX(node)) return true;
    }
    return false;
  }

  function hasReachableVerticalScroller(el) {
    for (let node = el && el.parentElement; node && node !== document.body && node !== document.documentElement; node = node.parentElement) {
      if (scrollsY(node)) {
        const r = node.getBoundingClientRect();
        if (r.bottom > 0 && r.top < (window.innerHeight || 0)) return true;
      }
    }
    return false;
  }

  function rootScrollLocked() {
    return [document.documentElement, document.body]
      .filter(Boolean)
      .map((node) => getComputedStyle(node).overflowY)
      .some((v) => v === "hidden" || v === "clip");
  }

  function paddingBox(el) {
    const r = el.getBoundingClientRect();
    const style = getComputedStyle(el);
    return {
      left: r.left + px(style.borderLeftWidth),
      right: r.right - px(style.borderRightWidth),
      top: r.top + px(style.borderTopWidth),
      bottom: r.bottom - px(style.borderBottomWidth),
    };
  }

  function textNodesOf(el) {
    const deep = isTextBoundary(el);
    const nodes = [];
    const pending = Array.from((el && el.childNodes) || []);
    while (pending.length > 0) {
      const node = pending.shift();
      if (!node) continue;
      if (node.nodeType === 3) {
        if (String(node.textContent || "").trim()) nodes.push(node);
      } else if (deep && node.nodeType === 1) {
        pending.unshift(...Array.from(node.childNodes || []));
      }
    }
    return nodes;
  }

  function textFragments(el) {
    const fragments = [];
    for (const textNode of textNodesOf(el)) {
      const range = document.createRange();
      range.selectNodeContents(textNode);
      for (const r of Array.from(range.getClientRects())) if (r.width > 0 && r.height > 0) fragments.push(r);
    }
    return fragments;
  }

  function isTruncationIntended(style) {
    return style.textOverflow === "ellipsis" || Number.parseInt(style.webkitLineClamp || "0", 10) > 0;
  }

  function hasMask(style) {
    const mask = String(style.maskImage || style.webkitMaskImage || "none").toLowerCase();
    const clip = String(style.clipPath || "none").toLowerCase();
    return (mask !== "none" && mask !== "") || (clip !== "none" && clip !== "");
  }

  function isRoundedMask(style) {
    const clips = ["hidden", "clip"].includes(style.overflowX) || ["hidden", "clip"].includes(style.overflowY);
    if (!clips) return false;
    return [style.borderTopLeftRadius, style.borderTopRightRadius, style.borderBottomRightRadius, style.borderBottomLeftRadius].some((v) => px(v) > 0);
  }

  function hasMaskAncestor(el) {
    for (let node = el; node && node.nodeType === 1; node = node.parentElement) {
      const style = getComputedStyle(node);
      if (hasMask(style) || isRoundedMask(style)) return true;
    }
    return false;
  }

  function clippingBoundaries(el) {
    const out = [];
    for (let node = el && el.parentElement; node && node !== document.body && node !== document.documentElement; node = node.parentElement) {
      const style = getComputedStyle(node);
      const axes = [];
      if (style.overflowX === "hidden" || style.overflowX === "clip") axes.push("horizontal");
      if (style.overflowY === "hidden" || style.overflowY === "clip") axes.push("vertical");
      if (axes.length > 0 && !hasMask(style) && !isRoundedMask(style)) out.push({ el: node, box: paddingBox(node), axes });
    }
    return out;
  }

  function isVisuallyHidden(el, style, rect) {
    const positioned = style.position === "absolute" || style.position === "fixed";
    const clipped = style.overflowX === "hidden" || style.overflowX === "clip";
    const legacy = String(style.clip || "").toLowerCase();
    const clipPath = String(style.clipPath || "").toLowerCase();
    const hasClip = legacy !== "auto" || (clipPath !== "none" && clipPath !== "");
    return positioned && clipped && rect.width <= 2 && rect.height <= 2 && (style.whiteSpace === "nowrap" || hasClip);
  }

  function hasVisuallyHiddenAncestor(el) {
    for (let node = el; node && node.nodeType === 1; node = node.parentElement) {
      if (isVisuallyHidden(node, getComputedStyle(node), node.getBoundingClientRect())) return true;
    }
    return false;
  }

  function isExcludedFromAudit(el) {
    return !!(el.closest && el.closest(".mermaid,svg,[data-redline-ui]")) || hasMaskAncestor(el) || hasVisuallyHiddenAncestor(el);
  }

  function edgeOverflow(fragment, box, axis) {
    const start = axis === "horizontal" ? fragment.left : fragment.top;
    const end = axis === "horizontal" ? fragment.right : fragment.bottom;
    const boxStart = axis === "horizontal" ? box.left : box.top;
    const boxEnd = axis === "horizontal" ? box.right : box.bottom;
    const size = Math.max(0, end - start);
    if (![start, end, boxStart, boxEnd].every(Number.isFinite) || size <= 0) return { overflow: 0, ratio: 0, centerOutside: false };
    const before = Math.max(0, boxStart - start);
    const after = Math.max(0, end - boxEnd);
    const center = start + size / 2;
    return { overflow: Math.max(before, after), ratio: Math.min(1, (before + after) / size), centerOutside: center < boxStart || center > boxEnd };
  }

  // A line is severe only when a material part of a real text fragment
  // crosses its clipping boundary, or a wrapped line spills well outside its
  // own box.
  function severeTextOverflow(fragments, box, overflowX, overflowY, truncated) {
    if (truncated || !box || fragments.length === 0) return null;
    const clipsX = overflowX === "hidden" || overflowX === "clip";
    const clipsY = overflowY === "hidden" || overflowY === "clip";
    const spillsY = overflowY === "visible";
    const scrollX = overflowX === "auto" || overflowX === "scroll";
    const scrollY = overflowY === "auto" || overflowY === "scroll";
    let strongest = null;
    for (const f of fragments) {
      const h = edgeOverflow(f, box, "horizontal");
      const v = edgeOverflow(f, box, "vertical");
      const severeX = clipsX && !scrollX && h.overflow > 1 && (h.centerOutside || h.ratio >= 0.2);
      const severeY = (clipsY || spillsY) && !scrollY && v.overflow > 1 && v.centerOutside;
      for (const c of [severeX ? { axis: "horizontal", overflowPx: h.overflow } : null, severeY ? { axis: "vertical", overflowPx: v.overflow } : null]) {
        if (c && (!strongest || c.overflowPx > strongest.overflowPx)) strongest = c;
      }
    }
    return strongest;
  }

  function materialEscape(rect, boundary, axes, minOutsidePx) {
    let strongest = null;
    for (const axis of axes) {
      const start = axis === "horizontal" ? rect.left : rect.top;
      const end = axis === "horizontal" ? rect.right : rect.bottom;
      const bStart = axis === "horizontal" ? boundary.left : boundary.top;
      const bEnd = axis === "horizontal" ? boundary.right : boundary.bottom;
      const size = Math.max(0, end - start);
      if (![start, end, bStart, bEnd].every(Number.isFinite) || size <= 0) continue;
      const before = Math.max(0, bStart - start);
      const after = Math.max(0, end - bEnd);
      const outside = Math.max(before, after);
      const center = start + size / 2;
      const centerOutside = center < bStart || center > bEnd;
      if (outside < (minOutsidePx || 4) || (!centerOutside && (before + after) / size < 0.2)) continue;
      const c = { axis, side: before >= after ? "start" : "end", overflowPx: outside };
      if (!strongest || c.overflowPx > strongest.overflowPx) strongest = c;
    }
    return strongest;
  }

  function animationTarget(animation) {
    const target = animation.effect && animation.effect.target;
    if (target instanceof Element) return target;
    return target && target.element instanceof Element ? target.element : null;
  }

  function runningAnimations() {
    if (typeof document.getAnimations !== "function") return [];
    return document.getAnimations().filter((a) => ["running", "pending"].includes(String(a.playState)) && !isRedlineUi(animationTarget(a)));
  }

  function movingTargets() {
    return runningAnimations().map(animationTarget).filter(Boolean);
  }

  function isMoving(el, targets) {
    return targets.some((t) => t === el || t.contains(el) || el.contains(t));
  }

  function auditLayout() {
    const width = window.innerWidth || document.documentElement.clientWidth || 0;
    const height = window.innerHeight || 0;
    const findings = [];
    const seen = new Set();
    const add = (selectorValue, kind, axis, overflowPx) => {
      const key = kind + ":" + selectorValue + ":" + axis;
      if (seen.has(key)) return;
      seen.add(key);
      findings.push({ selector: selectorValue, kind, axis, overflowPx: Math.round(Math.max(0, overflowPx) * 10) / 10, viewportWidth: Math.round(width), severity: "error" });
    };
    const elements = Array.from((document.body && document.body.querySelectorAll("*")) || []).filter((el) => !isRedlineUi(el)).slice(0, 800);
    const moving = movingTargets();
    const viewport = { left: 0, right: width, top: 0, bottom: height };
    const materialPx = Math.max(24, width * 0.05);

    // Content that materially escapes the page to the right.
    const escapes = (el) => {
      if (hasHorizontalScrollerAncestor(el) || isMoving(el, moving) || isExcludedFromAudit(el)) return false;
      if (!isTextBoundary(el) && hasTextBoundaryAncestor(el)) return false;
      const rect = el.getBoundingClientRect();
      if (!isVisibleForAudit(el, rect)) return false;
      const style = getComputedStyle(el);
      if (["absolute", "fixed", "sticky"].includes(style.position) && !isRequiredControl(el)) return false;
      if (isRequiredControl(el)) {
        const e = materialEscape(rect, viewport, ["horizontal"], 4);
        return !!(e && e.side === "end");
      }
      if (!auditedText(el)) return false;
      return textFragments(el).some((f) => {
        const e = materialEscape(f, viewport, ["horizontal"], materialPx);
        return !!(e && e.side === "end");
      });
    };
    const pageOverflow = document.documentElement.scrollWidth - width;
    if (pageOverflow >= Math.max(24, width * 0.05) && elements.some(escapes)) add("html", "page-horizontal-overflow", "horizontal", pageOverflow);

    const failedRoots = [];
    for (const el of elements) {
      // Required controls: cut off by a container, or out of reach.
      if (!isRequiredControl(el) || isExcludedFromAudit(el) || isMoving(el, moving)) continue;
      const rect = el.getBoundingClientRect();
      if (!isVisibleForAudit(el, rect)) continue;
      let clipped = null;
      for (const b of clippingBoundaries(el)) {
        const e = materialEscape(rect, b.box, b.axes, 4);
        if (e && (!clipped || e.overflowPx > clipped.e.overflowPx)) clipped = { b, e };
      }
      if (clipped && !failedRoots.some((root) => root === clipped.b.el || root.contains(clipped.b.el))) {
        failedRoots.push(clipped.b.el);
        add(selector(clipped.b.el), "clipped-control", clipped.e.axis, clipped.e.overflowPx);
      }
      const horizontal = hasHorizontalScrollerAncestor(el) ? null : materialEscape(rect, viewport, ["horizontal"], 4);
      if (horizontal && horizontal.side === "start") add(selector(el), "viewport-unreachable-control", "horizontal", horizontal.overflowPx);
      const style = getComputedStyle(el);
      const pinned = style.position === "fixed" || style.position === "sticky" || (rootScrollLocked() && !hasReachableVerticalScroller(el));
      const y = window.scrollY || 0;
      const vRect = pinned ? rect : { top: rect.top + y, bottom: rect.bottom + y, left: rect.left, right: rect.right };
      const vBound = pinned ? viewport : { top: 0, bottom: document.documentElement.scrollHeight, left: 0, right: width };
      const vertical = materialEscape(vRect, vBound, ["vertical"], 4);
      if (vertical) add(selector(el), "viewport-unreachable-control", "vertical", vertical.overflowPx);
    }

    for (const el of elements) {
      // Text pushed off the left edge, where nobody can scroll to it.
      if (hasHorizontalScrollerAncestor(el) || isMoving(el, moving) || isExcludedFromAudit(el)) continue;
      if (!isTextBoundary(el) && hasTextBoundaryAncestor(el)) continue;
      if (!auditedText(el)) continue;
      const rect = el.getBoundingClientRect();
      if (!isVisibleForAudit(el, rect)) continue;
      if (["absolute", "fixed", "sticky"].includes(getComputedStyle(el).position) && !isRequiredControl(el)) continue;
      let left = null;
      for (const f of textFragments(el)) {
        const e = materialEscape(f, viewport, ["horizontal"], materialPx);
        if (e && e.side === "start" && (!left || e.overflowPx > left.overflowPx)) left = e;
      }
      if (left) add(selector(el), "viewport-unreachable-content", "horizontal", left.overflowPx);
    }

    for (const el of elements) {
      // Text clipped by its own container or a clipping ancestor.
      if (el === document.body || el === document.documentElement || isExcludedFromAudit(el) || !auditedText(el)) continue;
      if (!isTextBoundary(el) && hasTextBoundaryAncestor(el)) continue;
      if (failedRoots.some((root) => root.contains(el)) || isMoving(el, moving)) continue;
      const rect = el.getBoundingClientRect();
      if (!isVisibleForAudit(el, rect)) continue;
      const style = getComputedStyle(el);
      const fragments = textFragments(el);
      const truncated = isTruncationIntended(style);
      let severe = severeTextOverflow(fragments, paddingBox(el), style.overflowX, style.overflowY, truncated);
      let root = el;
      for (const b of clippingBoundaries(el)) {
        const failure = severeTextOverflow(fragments, b.box, b.axes.includes("horizontal") ? "hidden" : "auto", b.axes.includes("vertical") ? "hidden" : "auto", truncated);
        if (failure && (!severe || failure.overflowPx > severe.overflowPx)) {
          severe = failure;
          root = b.el;
        }
      }
      if (!severe) continue;
      failedRoots.push(root);
      add(selector(root), "clipped-text", severe.axis, severe.overflowPx);
    }

    auditOcclusion(elements, width, height, moving, add);
    return findings;
  }

  function isOpaque(el) {
    const style = getComputedStyle(el);
    if (Number.parseFloat(style.opacity || "1") < 0.95) return false;
    const color = String(style.backgroundColor || "").trim().toLowerCase();
    const rgba = color.match(/^rgba?\(([^)]+)\)$/);
    if (!rgba) return false;
    const parts = rgba[1].split(/[\s,/]+/).filter(Boolean);
    if (parts.length < 4) return true;
    const alpha = Number(parts[3]);
    return Number.isFinite(alpha) && alpha >= 0.95;
  }

  function opacityTo(node, stop) {
    let opacity = 1;
    for (let n = node; n && n !== stop; n = n.parentElement) {
      const v = Number.parseFloat(getComputedStyle(n).opacity || "1");
      if (Number.isFinite(v)) opacity *= v;
    }
    return opacity;
  }

  // The opaque sibling (of the text or one of its ancestors) covering a
  // point, if any.
  function blockerAt(el, point, moving) {
    const top = document.elementFromPoint(point.x, point.y);
    if (!(top instanceof Element) || top === el || el.contains(top) || top.contains(el) || isRedlineUi(top)) return null;
    const ancestors = [];
    for (let n = el; n && n !== document.body && n !== document.documentElement; n = n.parentElement) ancestors.push(n);
    let opaque = false;
    for (let n = top; n && n !== document.body && n !== document.documentElement; n = n.parentElement) {
      if (isMoving(n, moving)) return null;
      if (isOpaque(n)) opaque = true;
      const sibling = ancestors.find((a) => a.parentElement === n.parentElement);
      if (sibling && opaque && opacityTo(top, n.parentElement) >= 0.95) return n;
    }
    return null;
  }

  function auditOcclusion(elements, width, height, moving, add) {
    const candidates = elements
      .filter((el) => !isExcludedFromAudit(el))
      .filter((el) => {
        const text = auditedText(el);
        return text.length >= 8 || (text.length > 0 && isRequiredControl(el));
      })
      .filter((el) => isTextBoundary(el) || !hasTextBoundaryAncestor(el))
      .filter((el) => isVisibleForAudit(el))
      .filter((el) => getComputedStyle(el).position === "static")
      .filter((el) => !isMoving(el, moving))
      .slice(0, 200);
    const failed = [];
    for (const el of candidates) {
      if (failed.some((root) => root.contains(el))) continue;
      const blockers = new Map();
      let total = 0;
      for (const f of textFragments(el)) {
        if (f.width * f.height < 16) continue;
        for (const xr of [0.2, 0.5, 0.8]) {
          for (const yr of [0.2, 0.5, 0.8]) {
            const point = { x: f.left + f.width * xr, y: f.top + f.height * yr };
            if (point.x < 0 || point.y < 0 || point.x > width || point.y > height) continue;
            total += 1;
            const b = blockerAt(el, point, moving);
            if (b) blockers.set(b, (blockers.get(b) || 0) + 1);
          }
        }
      }
      const occluded = Math.max(0, ...blockers.values());
      if (total < 5 || occluded / total < 0.9) continue;
      failed.push(el);
      add(selector(el), "overlapping-text", "horizontal", 0);
    }
  }

  function frames(count) {
    return new Promise((resolve) => {
      const step = (n) => (n <= 0 ? resolve() : requestAnimationFrame(() => step(n - 1)));
      step(count);
    });
  }

  function fontsReady() {
    try {
      if (document.fonts && document.fonts.ready) return document.fonts.ready.catch(() => {});
    } catch (_error) {
      // The resize settle below is the safety net.
    }
    return Promise.resolve();
  }

  // Resolves once nothing has resized for a moment (or after a cap).
  function resizeSettled() {
    return new Promise((resolve) => {
      let settle = 0;
      let observer = null;
      let done = false;
      const finish = () => {
        if (done) return;
        done = true;
        clearTimeout(settle);
        clearTimeout(cap);
        if (observer) observer.disconnect();
        resolve();
      };
      const again = () => {
        clearTimeout(settle);
        settle = setTimeout(finish, AUDIT_SETTLE_MS);
      };
      const cap = setTimeout(finish, AUDIT_MAX_WAIT_MS);
      if (typeof ResizeObserver !== "undefined") {
        observer = new ResizeObserver(again);
        for (const el of [document.documentElement, document.body, ...Array.from((document.body && document.body.querySelectorAll("*")) || [])].filter(Boolean).slice(0, 800)) {
          observer.observe(el);
        }
      }
      again();
    });
  }

  // Resolves true once the DOM stops changing, false if it keeps changing.
  function domQuiet() {
    return new Promise((resolve) => {
      if (typeof MutationObserver === "undefined") {
        resolve(false);
        return;
      }
      let settle = 0;
      let done = false;
      const finish = (quiet) => {
        if (done) return;
        done = true;
        clearTimeout(settle);
        clearTimeout(cap);
        observer.disconnect();
        resolve(quiet);
      };
      const observer = new MutationObserver((records) => {
        if (records.every((r) => isRedlineUi(r.target.nodeType === 1 ? r.target : r.target.parentElement))) return;
        clearTimeout(settle);
        settle = setTimeout(() => finish(true), AUDIT_SETTLE_MS);
      });
      observer.observe(document.documentElement, { attributes: true, characterData: true, childList: true, subtree: true });
      settle = setTimeout(() => finish(true), AUDIT_SETTLE_MS);
      const cap = setTimeout(() => finish(false), AUDIT_MAX_WAIT_MS);
    });
  }

  async function finiteAnimationsSettled() {
    const finite = runningAnimations().filter((a) => {
      const end = Number(a.effect && a.effect.getComputedTiming && a.effect.getComputedTiming().endTime);
      return Number.isFinite(end);
    });
    if (finite.length === 0) return true;
    let settled = false;
    await Promise.race([
      Promise.all(finite.map((a) => a.finished.catch(() => {}))).then(() => {
        settled = true;
      }),
      new Promise((resolve) => setTimeout(resolve, AUDIT_ANIMATION_MAX_WAIT_MS)),
    ]);
    if (!settled) for (const a of finite) a.finished.then(() => scheduleAudit(), () => scheduleAudit());
    return settled;
  }

  function stableFindings(first, second) {
    const key = (f) => f.kind + ":" + f.selector + ":" + f.axis;
    const keys = new Set(first.map(key));
    return second.filter((f) => keys.has(key(f)));
  }

  // A pass reports its own completeness: an incomplete pass is uncertainty,
  // never evidence that an earlier finding is gone.
  function publishAudit(findings, complete, targetPresenceComplete) {
    const viewportWidth = window.innerWidth || document.documentElement.clientWidth || 0;
    const signature = JSON.stringify({ complete, targetPresenceComplete, viewportWidth, findings });
    if (signature === lastAuditSignature) return;
    lastAuditSignature = signature;
    post("layout", { complete, targetPresenceComplete, viewportWidth, findings });
  }

  async function runAudit(run) {
    await fontsReady();
    await resizeSettled();
    const animationsSettled = await finiteAnimationsSettled();
    await frames(2);
    if (run !== auditRun) return;
    const first = auditLayout();
    await new Promise((resolve) => setTimeout(resolve, AUDIT_STABLE_SAMPLE_MS));
    await frames(2);
    if (run !== auditRun) return;
    const second = auditLayout();
    const quiet = await domQuiet();
    if (run !== auditRun) return;
    const last = quiet ? auditLayout() : second;
    const presence = document.readyState === "complete" && quiet;
    publishAudit(stableFindings(quiet ? second : first, last), animationsSettled && presence, presence);
  }

  function scheduleAudit() {
    clearTimeout(auditTimer);
    const run = ++auditRun;
    auditTimer = setTimeout(() => {
      runAudit(run).catch(() => {
        if (run === auditRun) publishAudit([], false, false);
      });
    }, 50);
  }

  function startAudit() {
    scheduleAudit();
    window.addEventListener("load", () => scheduleAudit(), { once: true });
    window.addEventListener("resize", () => scheduleAudit(), { passive: true });
    window.addEventListener("animationend", () => scheduleAudit(), { passive: true });
    window.addEventListener("transitionend", () => scheduleAudit(), { passive: true });
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
    startAudit();
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", ready, { once: true });
  else ready();
})();
