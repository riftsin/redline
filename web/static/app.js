// redline review page. Served as-is: no bundler, no dependencies.
//
// The server owns all review state. It renders the page, then pushes the
// regions that change (conversation, presence, banners) over the
// /events/{key} stream; this script swaps them in and turns the reviewer's
// actions into POSTs. The only state kept here is per tab and never
// re-rendered by the server: the interaction mode, the open annotation card,
// a note being edited, images being attached and the composer's text.
(() => {
  "use strict";

  const root = document.getElementById("review");
  if (!root) return;

  const key = root.dataset.key;
  const maxImages = Number(root.dataset.maxImages || 4);
  const frame = document.getElementById("artifact");
  const card = document.getElementById("annotation-card");
  const cardTitle = document.getElementById("card-title");
  const cardExcerpt = document.getElementById("card-excerpt");
  const note = document.getElementById("annotation-note");
  const composer = document.getElementById("composer");
  const composerInput = document.getElementById("composer-input");
  const composerError = document.getElementById("composer-error");
  const sendButton = document.getElementById("send");
  const sendEndButton = document.getElementById("send-end");
  const queueButton = card.querySelector("button[type=submit]");
  const panelScroll = document.getElementById("panel-scroll");
  const menuButton = document.getElementById("menu-button");
  const menu = document.getElementById("menu");
  const changesChip = document.getElementById("changes-chip");
  const changesLabel = document.getElementById("changes-label");
  const sidebarToggle = document.getElementById("sidebar-toggle");
  const sidebarBadge = document.getElementById("sidebar-badge");
  const fileName = document.querySelector(".file-name").textContent;
  const IS_MAC = /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent || "");

  if (IS_MAC) {
    const hint = document.getElementById("card-hint");
    hint.textContent = "⌘↵ also sends";
    hint.title = "Enter queues the note; ⌘↵ sends it now with everything queued";
  }

  const state = {
    ended: root.classList.contains("is-ended"),
    queued: Number(document.getElementById("conversation").dataset.queued || 0),
    revision: Number(root.dataset.revision || 0),
    mode: "annotate",
    // Once the reviewer picks a mode it wins over the artifact's default,
    // across reloads of the artifact.
    modeChosen: false,
    frameReady: false,
    scrollY: 0,
    restoreY: null,
    // The revision the frame shows, and whether it was reloaded because the
    // agent changed the file (then its changes are measured).
    shownRevision: Number(root.dataset.revision || 0),
    measure: false,
    pending: null, // what the open annotation card is attached to
    sending: false,
    confirmEnd: false,
    // A queued note being edited in place, and a conversation update that
    // arrived meanwhile (applied when the edit ends).
    editing: null,
    deferredConversation: null,
    changes: null, // {selectors, rewrite, index}
  };

  // -- Server calls -----------------------------------------------------------

  async function api(path, body, init) {
    const response = await fetch("/api/" + encodeURIComponent(key) + path, init || {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {}),
    });
    let data = {};
    try {
      data = await response.json();
    } catch (_error) {
      // A proxy or crash page; the status says enough.
    }
    if (!response.ok) {
      const error = new Error(data.error || "Request failed (" + response.status + ")");
      error.code = data.code;
      throw error;
    }
    return data;
  }

  function showError(message) {
    composerError.textContent = message;
    composerError.hidden = !message;
  }

  // -- Live updates -------------------------------------------------------------

  function connect() {
    const source = new EventSource("/events/" + encodeURIComponent(key));
    source.addEventListener("state", (event) => applyState(JSON.parse(event.data)));
    source.addEventListener("region", (event) => swapRegion(JSON.parse(event.data)));
    source.addEventListener("reload", (event) => {
      const data = JSON.parse(event.data);
      if (data.revision !== state.shownRevision) reloadFrame(data.src, true);
    });
    source.addEventListener("open", () => root.classList.remove("is-offline"));
    source.addEventListener("error", () => {
      // EventSource reconnects by itself; the server resyncs on reconnect.
      root.classList.add("is-offline");
    });
  }

  function applyState(next) {
    state.queued = next.queued;
    state.revision = next.revision;
    if (next.ended !== state.ended) {
      state.ended = next.ended;
      root.classList.toggle("is-ended", state.ended);
      if (state.ended) {
        closeCard();
        toFrame("set-mode", { mode: "explore" });
      } else {
        // Reopened (redline <file> --reopen): annotating works again in this
        // tab, in the mode the toolbar shows.
        toFrame("set-mode", { mode: state.mode });
      }
    }
    updateControls();
    updateSidebarBadge();
  }

  // A region replaced between pointer-down and pointer-up would swallow the
  // click (it would start and end on different elements), so an update to
  // the region being pressed waits for the release.
  let pressed = null;
  const deferredRegions = new Map();
  document.addEventListener("pointerdown", (event) => {
    pressed = event.target;
  }, true);
  const release = () => {
    // After the click that follows the release has been dispatched.
    setTimeout(() => {
      pressed = null;
      const pending = Array.from(deferredRegions.entries());
      deferredRegions.clear();
      for (const [id, html] of pending) swapRegion({ id, html });
    }, 0);
  };
  document.addEventListener("pointerup", release, true);
  document.addEventListener("pointercancel", release, true);

  function swapRegion({ id, html }) {
    if (id === "conversation" && state.editing) {
      // Never pull a note out from under the reviewer's cursor.
      state.deferredConversation = html;
      return;
    }
    const current = document.getElementById(id);
    if (!current) return;
    if (pressed && current.contains(pressed)) {
      deferredRegions.set(id, html);
      return;
    }
    const pinned = id === "conversation" && isPinned();
    const template = document.createElement("template");
    template.innerHTML = html.trim();
    const replacement = template.content.firstElementChild;
    if (!replacement) return;
    current.replaceWith(replacement);
    if (pinned) panelScroll.scrollTop = panelScroll.scrollHeight;
    if (id === "conversation") noteReplies(replacement);
    if (id === "layout") restoreLayoutSelection();
  }

  // A reply that arrives while the conversation is out of sight marks it.
  function noteReplies(conversation) {
    const replies = conversation.querySelectorAll(".bubble.agent").length;
    if (replies > agentReplies) {
      if (PHONE.matches && !sheetOpen()) document.documentElement.classList.add("has-unread");
      if (!PHONE.matches && root.classList.contains("panel-collapsed")) state.unread = true;
    }
    agentReplies = replies;
    updateSidebarBadge();
  }

  // The conversation follows new entries unless the reviewer scrolled up.
  function isPinned() {
    return panelScroll.scrollHeight - panelScroll.scrollTop - panelScroll.clientHeight < 24;
  }

  function updateControls() {
    const uploading = images.card.some((i) => i.status === "uploading") || images.composer.some((i) => i.status === "uploading");
    const disabled = state.ended || state.sending;
    composerInput.disabled = disabled;
    sendButton.disabled = disabled || uploading;
    sendEndButton.disabled = disabled || uploading;
    queueButton.disabled = uploading;
    sendButton.textContent = state.sending ? "Sending…" : uploading ? "Uploading…" : "Send to Agent";
    for (const button of root.querySelectorAll(".mode")) button.disabled = state.ended;
    const end = menu.querySelector("[data-action=end]");
    end.hidden = state.ended;
    end.textContent = state.confirmEnd ? "End with " + state.queued + " unsent?" : "End session";
  }

  // -- Artifact frame -----------------------------------------------------------

  function toFrame(type, payload) {
    // The frame's origin is opaque, so "*" is the only possible target.
    if (frame.contentWindow) frame.contentWindow.postMessage({ redline: true, type, payload: payload || {} }, "*");
  }

  function revisionOf(src) {
    const rev = Number(new URL(src, location.href).searchParams.get("rev"));
    return Number.isFinite(rev) ? rev : state.shownRevision;
  }

  function reloadFrame(src, measure) {
    // Keep the reviewer's place across the agent's edits.
    state.restoreY = state.scrollY;
    state.frameReady = false;
    state.measure = Boolean(measure);
    state.shownRevision = revisionOf(src);
    delete root.dataset.frameReady;
    frame.src = src;
  }

  function setMode(mode, chosen) {
    state.mode = mode === "explore" ? "explore" : "annotate";
    if (chosen) state.modeChosen = true;
    root.dataset.mode = state.mode;
    for (const button of root.querySelectorAll(".mode")) {
      const active = button.dataset.mode === state.mode;
      button.classList.toggle("active", active);
      button.setAttribute("aria-pressed", String(active));
    }
    toFrame("set-mode", { mode: state.mode });
    if (state.mode === "explore") closeCard();
  }

  const snapshotWaiters = new Map();
  let snapshotSeq = 0;

  // Asks the artifact for an outline of what the reviewer sees. Never holds
  // up a send for long: an unresponsive artifact yields no snapshot.
  function requestSnapshot() {
    if (!state.frameReady) return Promise.resolve("");
    const id = ++snapshotSeq;
    return new Promise((resolve) => {
      const timer = setTimeout(() => {
        snapshotWaiters.delete(id);
        resolve("");
      }, 800);
      snapshotWaiters.set(id, (snapshot) => {
        clearTimeout(timer);
        resolve(snapshot);
      });
      toFrame("request-snapshot", { id });
    });
  }

  // After the agent changed the file, the artifact compares the version the
  // reviewer saw with the new one and flashes what changed.
  async function measureChanges(revision) {
    let sources;
    try {
      sources = await (await fetch("/api/" + encodeURIComponent(key) + "/sources")).json();
    } catch (_error) {
      return;
    }
    if (!sources.previous || sources.revision !== revision) return;
    toFrame("diff", { revision, previous: sources.previous, current: sources.current });
  }

  function showChanges(payload) {
    const selectors = Array.isArray(payload.selectors) ? payload.selectors.filter((s) => typeof s === "string") : [];
    const rewrite = Boolean(payload.rewrite);
    if (!rewrite && selectors.length === 0) {
      changesChip.hidden = true;
      state.changes = null;
    } else {
      state.changes = { selectors, rewrite, index: 0 };
      renderChangesChip();
      changesChip.hidden = false;
    }
    api("/changes", { revision: payload.revision, selectors, rewrite }).catch(() => {});
  }

  function renderChangesChip() {
    const c = state.changes;
    changesChip.classList.toggle("is-rewrite", c.rewrite);
    changesChip.disabled = c.rewrite;
    // The number stays visible on a phone; the words may be hidden there.
    const label = (count, words) => {
      const n = document.createElement("span");
      n.className = "changes-count";
      n.textContent = count;
      const w = document.createElement("span");
      w.className = "changes-words";
      w.textContent = words;
      changesLabel.replaceChildren(n, w);
    };
    if (c.rewrite) changesLabel.textContent = "Page rewritten";
    else if (c.index === 0) label(String(c.selectors.length), c.selectors.length === 1 ? " change" : " changes");
    else label(c.index + "/" + c.selectors.length, " changes");
    changesChip.setAttribute("aria-label", c.rewrite ? "Page rewritten" : c.index === 0 ? c.selectors.length + " changes" : "Change " + c.index + " of " + c.selectors.length);
  }

  changesChip.addEventListener("click", () => {
    const c = state.changes;
    if (!c || c.rewrite || c.selectors.length === 0) return;
    c.index = (c.index % c.selectors.length) + 1;
    renderChangesChip();
    toFrame("reveal", { selector: c.selectors[c.index - 1] });
  });

  // -- Tab title and icon ---------------------------------------------------------

  const iconLink = document.querySelector("link[rel=icon]");
  const defaultIcon = iconLink.getAttribute("href");
  const meta = { loadTitle: "", loadIcon: "", title: null, icon: null };

  // Only the artifact's own files or inline images may become the tab icon.
  function safeIcon(value) {
    if (typeof value !== "string" || value === "") return "";
    if (/^data:image\/(png|gif|jpeg|webp|svg\+xml|x-icon|vnd\.microsoft\.icon)[;,]/i.test(value)) return value;
    try {
      const url = new URL(value, location.href);
      if (url.origin === location.origin && url.pathname.startsWith("/artifact/" + key + "/")) return url.pathname + url.search;
    } catch (_error) {
      // Not a URL.
    }
    return "";
  }

  function applyMeta() {
    const title = (meta.title !== null ? meta.title : meta.loadTitle).trim();
    document.title = (title || fileName) + " · redline";
    iconLink.setAttribute("href", (meta.icon !== null ? meta.icon : meta.loadIcon) || defaultIcon);
  }

  function updateMeta(payload) {
    if ("title" in payload) {
      const t = payload.title;
      meta.title = t === null || (typeof t === "string" && t.trim() === "") ? null : String(t).slice(0, 200);
    }
    if ("icon" in payload) meta.icon = payload.icon === null ? null : safeIcon(payload.icon) || null;
    applyMeta();
  }

  window.addEventListener("message", (event) => {
    if (event.source !== frame.contentWindow) return;
    const data = event.data;
    if (!data || data.redline !== true || typeof data.type !== "string") return;
    const payload = data.payload && typeof data.payload === "object" ? data.payload : {};

    switch (data.type) {
      case "ready": {
        const firstReady = !state.frameReady;
        state.frameReady = true;
        root.dataset.frameReady = "true";
        setMode(state.modeChosen ? state.mode : payload.annotate, false);
        if (state.ended) toFrame("set-mode", { mode: "explore" });
        if (state.restoreY !== null) {
          toFrame("restore-scroll", { y: state.restoreY });
          state.restoreY = null;
        }
        meta.loadTitle = typeof payload.title === "string" ? payload.title.slice(0, 200) : "";
        meta.loadIcon = safeIcon(payload.icon);
        applyMeta();
        if (firstReady && state.measure) {
          state.measure = false;
          measureChanges(state.shownRevision);
        }
        break;
      }
      case "scroll":
        state.scrollY = Number(payload.y) || 0;
        break;
      case "select-element":
      case "select-text":
        if (!state.ended) openCard(payload, data.type === "select-text");
        break;
      case "queue-prompt":
        if (!state.ended) queueAnswer(payload);
        break;
      case "snapshot": {
        const waiter = snapshotWaiters.get(payload.id);
        if (waiter) {
          snapshotWaiters.delete(payload.id);
          waiter(String(payload.snapshot || ""));
        }
        break;
      }
      case "changes":
        if (payload.revision === state.shownRevision) showChanges(payload);
        break;
      case "meta":
        updateMeta(payload);
        break;
      case "layout":
        // An audit pass of what is on screen. Detection is passive: it only
        // updates the inbox.
        api("/layout", {
          revision: state.shownRevision,
          complete: Boolean(payload.complete),
          target_presence_complete: Boolean(payload.targetPresenceComplete),
          viewport_width: Number(payload.viewportWidth) || 0,
          findings: Array.isArray(payload.findings) ? payload.findings.slice(0, 200) : [],
        }).catch(() => {});
        break;
      case "toggle-mode":
        if (!state.ended) setMode(state.mode === "annotate" ? "explore" : "annotate", true);
        break;
      case "send":
        send(false);
        break;
      case "end-session":
        endSession();
        break;
      case "asset-failure":
        api("/artifact-failures", {
          failures: [{ kind: "asset-failed", detail: String(payload.url || "a local asset") + " failed to load" }],
        }).catch(() => {});
        break;
    }
  });

  // -- Images ---------------------------------------------------------------------
  //
  // Images are uploaded as soon as they are attached; a note or message
  // carries the references. "card" is the annotation card, "composer" the
  // message box.

  const IMAGE_TYPES = ["image/png", "image/jpeg", "image/webp"];
  const images = { card: [], composer: [] };
  let imageSeq = 0;

  function readyRefs(where) {
    return images[where].filter((i) => i.status === "ready").map((i) => ({ id: i.id, name: i.name }));
  }

  function clearImages(where) {
    for (const i of images[where]) if (i.preview) URL.revokeObjectURL(i.preview);
    images[where] = [];
    renderChips(where);
  }

  function renderChips(where) {
    const box = document.querySelector("[data-chips=" + where + "]");
    box.textContent = "";
    for (const img of images[where]) {
      const chip = document.createElement("div");
      chip.className = "chip" + (img.status === "error" ? " chip-error" : "");
      const thumb = document.createElement("img");
      thumb.className = "chip-thumb";
      thumb.alt = "";
      if (img.preview) thumb.src = img.preview;
      const name = document.createElement("span");
      name.className = "chip-name";
      name.textContent = img.name;
      name.title = img.name;
      chip.append(thumb, name);
      if (img.status !== "ready") {
        const status = document.createElement("span");
        status.className = "chip-status";
        status.textContent = img.status === "uploading" ? "Uploading…" : img.message;
        chip.append(status);
      }
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "chip-remove";
      remove.setAttribute("aria-label", "Remove " + img.name);
      remove.textContent = "×";
      remove.addEventListener("click", () => {
        if (img.preview) URL.revokeObjectURL(img.preview);
        images[where] = images[where].filter((i) => i !== img);
        renderChips(where);
        updateControls();
      });
      chip.append(remove);
      box.append(chip);
    }
    box.hidden = images[where].length === 0;
  }

  async function addImages(where, files) {
    for (const file of files) {
      if (images[where].length >= maxImages) {
        showError("Attach at most " + maxImages + " images to one note or message.");
        break;
      }
      const img = { local: ++imageSeq, name: file.name || "pasted image", status: "uploading", preview: URL.createObjectURL(file) };
      if (!IMAGE_TYPES.includes(file.type)) {
        img.status = "error";
        img.message = "Not a PNG, JPEG or WebP image";
      }
      images[where].push(img);
      renderChips(where);
      updateControls();
      if (img.status === "error") continue;
      try {
        const data = await api("/attachments", null, { method: "POST", body: file });
        img.id = data.id;
        img.status = "ready";
      } catch (error) {
        img.status = "error";
        img.message = error.message;
      }
      renderChips(where);
      updateControls();
    }
  }

  function imageFiles(list) {
    return Array.from(list || []).filter((f) => f.type.startsWith("image/"));
  }

  for (const input of root.querySelectorAll("[data-attach]")) {
    input.addEventListener("change", () => {
      addImages(input.dataset.attach, Array.from(input.files || []));
      input.value = "";
    });
  }

  // Pasting images into a text box attaches them. Text pasted alongside is
  // kept; a copied file's own name, which some systems put on the clipboard
  // as text, is not.
  for (const box of root.querySelectorAll("textarea[data-images]")) {
    box.addEventListener("paste", (event) => {
      const files = imageFiles(event.clipboardData && event.clipboardData.files);
      if (files.length === 0) return;
      addImages(box.dataset.images, files);
      const text = (event.clipboardData.getData("text/plain") || "").trim();
      if (text === "" || files.some((f) => f.name && text.endsWith(f.name))) event.preventDefault();
    });
  }

  for (const [form, where] of [[card, "card"], [composer, "composer"]]) {
    form.addEventListener("dragover", (event) => {
      if (!event.dataTransfer || !Array.from(event.dataTransfer.types).includes("Files")) return;
      event.preventDefault();
      form.classList.add("is-drop-target");
    });
    form.addEventListener("dragleave", (event) => {
      if (!form.contains(event.relatedTarget)) form.classList.remove("is-drop-target");
    });
    form.addEventListener("drop", (event) => {
      form.classList.remove("is-drop-target");
      const files = imageFiles(event.dataTransfer && event.dataTransfer.files);
      if (files.length === 0) return;
      event.preventDefault();
      addImages(where, files);
    });
  }

  // -- Annotation card ----------------------------------------------------------

  function openCard(target, isText) {
    state.pending = target;
    let title = isText ? "Annotate text" : "Annotate <" + (target.tag || "element") + ">";
    const cell = target.target && target.target.type === "table-cell" ? target.target : null;
    if (target.target && target.target.type === "mermaid-node") {
      title = "Annotate diagram node" + (target.target.label ? ": " + target.target.label : "");
    } else if (cell) {
      const labels = [cell.rowLabel, cell.columnLabel].filter((l) => l);
      title = labels.length ? "Annotate cell: " + labels.join(" → ") : "Annotate cell";
    }
    cardTitle.textContent = title;
    cardExcerpt.textContent = target.text && !(target.target && target.target.type === "mermaid-node") ? "“" + target.text + "”" : "";
    card.hidden = false;
    if (PHONE.matches) setSheet(true);
    if (root.classList.contains("panel-collapsed")) setPanel(true);
    note.focus();
  }

  function closeCard() {
    if (card.hidden) return;
    card.hidden = true;
    note.value = "";
    state.pending = null;
    clearImages("card");
    toFrame("clear-selection");
  }

  async function queueNote() {
    const target = state.pending;
    const prompt = note.value.trim();
    const refs = readyRefs("card");
    if (!target || (!prompt && refs.length === 0)) {
      note.focus();
      return;
    }
    if (images.card.some((i) => i.status === "uploading")) return;
    try {
      await api("/queue", {
        kind: "annotation",
        prompt,
        selector: target.selector || "",
        tag: target.tag || "",
        text: target.text || "",
        target: target.target || null,
        attachments: refs,
      });
      closeCard();
      showError("");
    } catch (error) {
      showError(error.message);
    }
  }

  function queueAnswer(item) {
    api("/queue", {
      kind: "answer",
      prompt: String(item.prompt || ""),
      selector: String(item.selector || ""),
      tag: String(item.tag || ""),
      text: String(item.text || ""),
      target: item.target || null,
      queueKey: String(item.queueKey || ""),
    }).catch((error) => showError(error.message));
  }

  card.addEventListener("submit", (event) => {
    event.preventDefault();
    queueNote();
  });
  document.getElementById("card-cancel").addEventListener("click", closeCard);

  // Enter queues the note; Shift+Enter is a newline; Cmd/Ctrl+Enter sends
  // it with everything queued; Escape cancels. The Enter that confirms an input
  // method composition never queues.
  note.addEventListener("keydown", (event) => {
    if (event.isComposing || event.keyCode === 229) return;
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      send(false);
    } else if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      queueNote();
    } else if (event.key === "Escape") {
      event.preventDefault();
      closeCard();
    }
  });

  // -- Composer -----------------------------------------------------------------

  // An element selected in the artifact travels with the send even when its
  // note was never queued: the selection already says what is meant. With
  // an empty note card, the message box text is the note on that element.
  async function send(end) {
    if (state.sending || state.ended) return;
    if (images.composer.some((i) => i.status === "uploading") || images.card.some((i) => i.status === "uploading")) return;
    state.sending = true;
    updateControls();
    const pending = state.pending;
    let message = composerInput.value;
    let attachments = readyRefs("composer");
    let selection = null;
    if (pending) {
      selection = {
        prompt: note.value.trim(),
        attachments: readyRefs("card"),
        selector: pending.selector || "",
        tag: pending.tag || "",
        text: pending.text || "",
        target: pending.target || null,
      };
      if (!selection.prompt && selection.attachments.length === 0) {
        selection.prompt = message.trim();
        selection.attachments = attachments;
        message = "";
        attachments = [];
      }
    }
    try {
      const snapshot = await requestSnapshot();
      await api("/send", { message, attachments, snapshot, end, selection });
      composerInput.value = "";
      clearImages("composer");
      if (pending && state.pending === pending) closeCard();
      showError("");
    } catch (error) {
      showError(error.message);
    } finally {
      state.sending = false;
      updateControls();
    }
  }

  composer.addEventListener("submit", (event) => {
    event.preventDefault();
    send(false);
  });
  sendEndButton.addEventListener("click", () => send(true));

  // Enter sends; Shift+Enter is a newline.
  composerInput.addEventListener("keydown", (event) => {
    if (event.isComposing || event.keyCode === 229) return;
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      send(false);
    }
  });

  async function endSession() {
    try {
      await api("/end");
    } catch (error) {
      showError(error.message);
    }
  }

  // -- Conversation -------------------------------------------------------------

  // Editing a queued note happens in place: Enter saves, Shift+Enter is a
  // newline, Escape discards the edit.
  function startEdit(id) {
    if (state.editing) return;
    const bubble = panelScroll.querySelector(".queued-bubble[data-id='" + CSS.escape(id) + "']");
    const text = bubble && bubble.querySelector("[data-prompt]");
    if (!text) return;
    const box = document.createElement("textarea");
    box.className = "edit-note";
    box.rows = 3;
    box.value = text.textContent;
    box.setAttribute("aria-label", "Edit note");
    const hint = document.createElement("p");
    hint.className = "edit-hint";
    hint.textContent = "Enter saves · Esc discards";
    text.replaceWith(box);
    box.after(hint);
    bubble.classList.add("is-editing");
    state.editing = { id, box };
    box.focus();
    box.setSelectionRange(box.value.length, box.value.length);

    const finish = () => {
      state.editing = null;
      if (state.deferredConversation !== null) {
        const html = state.deferredConversation;
        state.deferredConversation = null;
        swapRegion({ id: "conversation", html });
      } else {
        bubble.classList.remove("is-editing");
        hint.remove();
        box.replaceWith(text);
      }
    };
    box.addEventListener("keydown", async (event) => {
      if (event.isComposing || event.keyCode === 229) return;
      if (event.key === "Escape") {
        event.preventDefault();
        event.stopPropagation();
        finish();
      } else if (event.key === "Enter" && !event.shiftKey) {
        event.preventDefault();
        const prompt = box.value.trim();
        if (!prompt) return;
        try {
          await api("/queue/" + encodeURIComponent(id) + "/edit", { prompt });
          text.textContent = prompt;
          showError("");
        } catch (error) {
          showError(error.message);
        }
        finish();
      }
    });
    box.addEventListener("blur", () => {
      if (state.editing && state.editing.box === box) finish();
    });
  }

  panelScroll.addEventListener("click", (event) => {
    const remove = event.target.closest("[data-remove]");
    if (remove) {
      api("/queue/" + encodeURIComponent(remove.dataset.remove) + "/remove").catch((error) => showError(error.message));
      return;
    }
    const edit = event.target.closest("[data-edit]");
    if (edit) {
      startEdit(edit.dataset.edit);
      return;
    }
    const changes = event.target.closest("[data-changes]");
    if (changes) {
      let selectors = [];
      try {
        selectors = JSON.parse(changes.dataset.changes);
      } catch (_error) {
        // Nothing to show.
      }
      toFrame("flash", { selectors });
      return;
    }
    const anchor = event.target.closest(".anchor");
    if (anchor && anchor.dataset.selector) toFrame("reveal", { selector: anchor.dataset.selector });
  });

  // -- Layout issues inbox ---------------------------------------------------------
  //
  // The server renders the drawer; whether it is open and which issues are
  // selected belong to this tab and survive re-renders.

  const layoutSelected = new Set();

  function setLayoutDrawer(open) {
    root.classList.toggle("layout-open", open);
    const button = document.getElementById("layout-button");
    if (button) button.setAttribute("aria-expanded", String(open));
  }

  function restoreLayoutSelection() {
    const box = document.getElementById("layout");
    const boxes = Array.from(box.querySelectorAll(".layout-check"));
    const present = new Set(boxes.map((b) => b.value));
    for (const id of Array.from(layoutSelected)) if (!present.has(id)) layoutSelected.delete(id);
    for (const b of boxes) b.checked = layoutSelected.has(b.value);
    const all = box.querySelector("[data-layout-all]");
    if (all) all.checked = boxes.length > 0 && boxes.every((b) => b.checked);
    const queue = box.querySelector("[data-layout-queue]");
    if (queue) {
      queue.disabled = layoutSelected.size === 0 || state.ended;
      queue.textContent = layoutSelected.size > 1 ? "Queue " + layoutSelected.size + " fixes" : "Queue selected fixes";
    }
    if (box.dataset.active === "0" && box.querySelectorAll(".layout-item").length === 0) setLayoutDrawer(false);
  }

  root.addEventListener("click", (event) => {
    const box = document.getElementById("layout");
    if (event.target.closest("#layout-button")) {
      setLayoutDrawer(!root.classList.contains("layout-open"));
      return;
    }
    if (!box.contains(event.target)) {
      if (root.classList.contains("layout-open") && !event.target.closest(".layout-drawer")) setLayoutDrawer(false);
      return;
    }
    const reveal = event.target.closest("[data-layout-reveal]");
    if (reveal) {
      toFrame("reveal", { selector: reveal.dataset.layoutReveal });
      return;
    }
    const dismiss = event.target.closest("[data-layout-dismiss]");
    if (dismiss) {
      layoutSelected.delete(dismiss.dataset.layoutDismiss);
      api("/layout/" + encodeURIComponent(dismiss.dataset.layoutDismiss) + "/dismiss").catch((error) => showError(error.message));
      return;
    }
    if (event.target.closest("[data-layout-queue]")) {
      const ids = Array.from(layoutSelected);
      if (ids.length === 0) return;
      api("/layout/queue", { ids })
        .then(() => {
          layoutSelected.clear();
          setLayoutDrawer(false);
          showError("");
        })
        .catch((error) => showError(error.message));
    }
  });

  root.addEventListener("change", (event) => {
    const box = document.getElementById("layout");
    if (!box.contains(event.target)) return;
    if (event.target.matches("[data-layout-all]")) {
      for (const b of box.querySelectorAll(".layout-check")) {
        if (event.target.checked) layoutSelected.add(b.value);
        else layoutSelected.delete(b.value);
      }
    } else if (event.target.matches(".layout-check")) {
      if (event.target.checked) layoutSelected.add(event.target.value);
      else layoutSelected.delete(event.target.value);
    }
    restoreLayoutSelection();
  });

  // -- Phone bottom sheet ---------------------------------------------------------
  //
  // Under 860px the conversation is a bottom sheet over the artifact. Its
  // state is a class on <html>, which region swaps never touch.

  const PHONE = window.matchMedia("(max-width: 859px)");
  const dock = document.getElementById("sheet-dock");
  const sheetKey = "redline:sheet:" + key;
  let agentReplies = document.querySelectorAll("#conversation .bubble.agent").length;

  function sheetOpen() {
    return document.documentElement.classList.contains("sheet-open");
  }

  function setSheet(open) {
    document.documentElement.classList.toggle("sheet-open", open);
    dock.setAttribute("aria-expanded", String(open));
    if (open) document.documentElement.classList.remove("has-unread");
    remember(sheetKey, open ? "open" : "closed");
  }

  function remember(name, value) {
    try {
      localStorage.setItem(name, value);
    } catch (_error) {
      // Not remembered across reloads; nothing else depends on it.
    }
  }

  function recall(name) {
    try {
      return localStorage.getItem(name);
    } catch (_error) {
      return null;
    }
  }

  if (PHONE.matches && recall(sheetKey) === "open") setSheet(true);

  // Tap the dock to toggle, the dimmed artifact to close; swipe the dock up
  // to open and down to close.
  let swipeStart = null;
  let swiped = false;
  dock.addEventListener("pointerdown", (event) => {
    swipeStart = event.clientY;
  });
  dock.addEventListener("pointerup", (event) => {
    if (swipeStart === null) return;
    const dy = event.clientY - swipeStart;
    swipeStart = null;
    if (Math.abs(dy) < 30) return;
    swiped = true;
    setSheet(dy < 0);
  });
  dock.addEventListener("click", () => {
    if (swiped) swiped = false;
    else setSheet(!sheetOpen());
  });
  document.getElementById("sheet-scrim").addEventListener("click", () => setSheet(false));

  // -- Collapsible conversation (desktop) -------------------------------------

  const panelKey = "redline:panel:" + key;

  function setPanel(open) {
    root.classList.toggle("panel-collapsed", !open);
    sidebarToggle.setAttribute("aria-expanded", String(open));
    sidebarToggle.title = open ? "Hide the conversation" : "Show the conversation";
    if (open) state.unread = false;
    updateSidebarBadge();
    remember(panelKey, open ? "open" : "collapsed");
  }

  // While hidden, the toggle counts queued notes or marks an unread reply.
  function updateSidebarBadge() {
    const collapsed = root.classList.contains("panel-collapsed");
    if (!collapsed || (state.queued === 0 && !state.unread)) {
      sidebarBadge.hidden = true;
      return;
    }
    sidebarBadge.hidden = false;
    sidebarBadge.classList.toggle("is-dot", state.queued === 0);
    sidebarBadge.textContent = state.queued > 0 ? String(state.queued) : "";
    sidebarToggle.setAttribute("aria-label", state.queued > 0 ? state.queued + " queued notes; show the conversation" : "New reply; show the conversation");
  }

  sidebarToggle.addEventListener("click", () => setPanel(root.classList.contains("panel-collapsed")));
  if (recall(panelKey) === "collapsed") setPanel(false);

  // -- Top bar ------------------------------------------------------------------

  for (const button of root.querySelectorAll(".mode")) {
    button.addEventListener("click", () => setMode(button.dataset.mode, true));
  }

  function setMenu(open) {
    menu.hidden = !open;
    menuButton.setAttribute("aria-expanded", String(open));
    if (!open && state.confirmEnd) {
      state.confirmEnd = false;
      updateControls();
    }
  }

  menuButton.addEventListener("click", (event) => {
    event.stopPropagation();
    setMenu(menu.hidden);
  });
  document.addEventListener("click", (event) => {
    if (!menu.hidden && !event.target.closest(".menu")) setMenu(false);
  });

  menu.addEventListener("click", (event) => {
    const item = event.target.closest("[data-action]");
    if (!item) return;
    switch (item.dataset.action) {
      case "copy-path":
        if (navigator.clipboard) navigator.clipboard.writeText(document.querySelector(".file-name").title).catch(() => {});
        setMenu(false);
        break;
      case "export":
        // The link downloads; just close the menu.
        setMenu(false);
        break;
      case "reload":
        reloadFrame(root.dataset.artifact + "?rev=" + state.revision + "&t=" + Date.now(), false);
        setMenu(false);
        break;
      case "end":
        // Ending with unsent notes asks once more.
        if (state.queued > 0 && !state.confirmEnd) {
          state.confirmEnd = true;
          updateControls();
          return;
        }
        state.confirmEnd = false;
        setMenu(false);
        endSession();
        break;
    }
  });

  // Cmd/Ctrl+I toggles annotate/explore from anywhere on the page (the
  // artifact forwards the same shortcut); Escape closes the menu or sheet.
  window.addEventListener(
    "keydown",
    (event) => {
      const k = String(event.key || "").toLowerCase();
      if ((event.metaKey || event.ctrlKey) && !event.shiftKey && !event.altKey && k === "i") {
        event.preventDefault();
        if (!state.ended) setMode(state.mode === "annotate" ? "explore" : "annotate", true);
      } else if (event.key === "Escape" && root.classList.contains("layout-open")) {
        setLayoutDrawer(false);
      } else if (event.key === "Escape" && !menu.hidden) {
        setMenu(false);
      } else if (event.key === "Escape" && sheetOpen()) {
        const active = document.activeElement;
        const typing = active && active.tagName === "TEXTAREA" && active.value.trim() !== "";
        if (!typing) setSheet(false);
      }
    },
    true,
  );

  state.shownRevision = revisionOf(frame.src);

  panelScroll.scrollTop = panelScroll.scrollHeight;
  updateControls();
  applyMeta();
  connect();
  // The artifact may have loaded (and said "ready") before this script ran.
  toFrame("hello");
})();
