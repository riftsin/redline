// redline review page. Served as-is: no bundler, no dependencies.
//
// The server owns all review state. It renders the page, then pushes the
// regions that change (conversation, presence, banners) over the
// /events/{key} stream; this script swaps them in and turns the reviewer's
// actions into POSTs. The only state kept here is per tab and never
// re-rendered by the server: the interaction mode, the open annotation card
// and the composer's text.
(() => {
  "use strict";

  const root = document.getElementById("review");
  if (!root) return;

  const key = root.dataset.key;
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
  const panelScroll = document.getElementById("panel-scroll");
  const menuButton = document.getElementById("menu-button");
  const menu = document.getElementById("menu");

  if (/Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent || "")) {
    const hint = document.getElementById("card-hint");
    hint.textContent = "⌘↵ also sends";
    hint.title = "Enter queues the note; ⌘↵ queues it and sends everything";
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
    pending: null, // what the open annotation card is attached to
    sending: false,
    confirmEnd: false,
  };

  // -- Server calls -----------------------------------------------------------

  async function api(path, body) {
    const response = await fetch("/api/" + encodeURIComponent(key) + path, {
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
    source.addEventListener("reload", (event) => reloadFrame(JSON.parse(event.data).src));
    source.addEventListener("open", () => root.classList.remove("is-offline"));
    source.addEventListener("error", () => {
      // EventSource reconnects by itself; the server resyncs on reconnect.
      root.classList.add("is-offline");
    });
  }

  function applyState(next) {
    state.queued = next.queued;
    if (next.ended !== state.ended) {
      state.ended = next.ended;
      root.classList.toggle("is-ended", state.ended);
      if (state.ended) {
        closeCard();
        toFrame("set-mode", { mode: "explore" });
      }
    }
    updateControls();
  }

  function swapRegion({ id, html }) {
    const current = document.getElementById(id);
    if (!current) return;
    const pinned = id === "conversation" && isPinned();
    const template = document.createElement("template");
    template.innerHTML = html.trim();
    const replacement = template.content.firstElementChild;
    if (!replacement) return;
    current.replaceWith(replacement);
    if (pinned) panelScroll.scrollTop = panelScroll.scrollHeight;
    if (id === "conversation") {
      const replies = replacement.querySelectorAll(".bubble.agent").length;
      if (replies > agentReplies && PHONE.matches && !sheetOpen()) document.documentElement.classList.add("has-unread");
      agentReplies = replies;
    }
  }

  // The conversation follows new entries unless the reviewer scrolled up.
  function isPinned() {
    return panelScroll.scrollHeight - panelScroll.scrollTop - panelScroll.clientHeight < 24;
  }

  function updateControls() {
    const disabled = state.ended || state.sending;
    composerInput.disabled = disabled;
    sendButton.disabled = disabled;
    sendEndButton.disabled = disabled;
    sendButton.textContent = state.sending ? "Sending…" : "Send to Agent";
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

  function reloadFrame(src) {
    // Keep the reviewer's place across the agent's edits.
    state.restoreY = state.scrollY;
    state.frameReady = false;
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

  window.addEventListener("message", (event) => {
    if (event.source !== frame.contentWindow) return;
    const data = event.data;
    if (!data || data.redline !== true || typeof data.type !== "string") return;
    const payload = data.payload && typeof data.payload === "object" ? data.payload : {};

    switch (data.type) {
      case "ready":
        state.frameReady = true;
        root.dataset.frameReady = "true";
        setMode(state.modeChosen ? state.mode : payload.annotate, false);
        if (state.ended) toFrame("set-mode", { mode: "explore" });
        if (state.restoreY !== null) {
          toFrame("restore-scroll", { y: state.restoreY });
          state.restoreY = null;
        }
        break;
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

  // -- Annotation card ----------------------------------------------------------

  function openCard(target, isText) {
    state.pending = target;
    cardTitle.textContent = isText ? "Annotate text" : "Annotate <" + (target.tag || "element") + ">";
    cardExcerpt.textContent = target.text ? "“" + target.text + "”" : "";
    card.hidden = false;
    if (PHONE.matches) setSheet(true);
    note.focus();
  }

  function closeCard() {
    if (card.hidden) return;
    card.hidden = true;
    note.value = "";
    state.pending = null;
    toFrame("clear-selection");
  }

  async function queueNote(andSend) {
    const target = state.pending;
    const prompt = note.value.trim();
    if (!target || !prompt) {
      note.focus();
      return;
    }
    try {
      await api("/queue", {
        kind: "annotation",
        prompt,
        selector: target.selector || "",
        tag: target.tag || "",
        text: target.text || "",
        target: target.target || null,
      });
      closeCard();
      showError("");
      if (andSend) await send(false);
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
    queueNote(false);
  });
  document.getElementById("card-cancel").addEventListener("click", closeCard);

  // Enter queues the note; Shift+Enter is a newline; Cmd/Ctrl+Enter queues
  // and sends everything; Escape cancels. The Enter that confirms an input
  // method composition never queues.
  note.addEventListener("keydown", (event) => {
    if (event.isComposing || event.keyCode === 229) return;
    if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
      event.preventDefault();
      queueNote(true);
    } else if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      queueNote(false);
    } else if (event.key === "Escape") {
      event.preventDefault();
      closeCard();
    }
  });

  // -- Composer -----------------------------------------------------------------

  async function send(end) {
    if (state.sending || state.ended) return;
    state.sending = true;
    updateControls();
    try {
      const snapshot = await requestSnapshot();
      await api("/send", { message: composerInput.value, snapshot, end });
      composerInput.value = "";
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

  panelScroll.addEventListener("click", (event) => {
    const remove = event.target.closest("[data-remove]");
    if (remove) {
      api("/queue/" + encodeURIComponent(remove.dataset.remove) + "/remove").catch((error) => showError(error.message));
      return;
    }
    const anchor = event.target.closest(".anchor");
    if (anchor && anchor.dataset.selector) toFrame("reveal", { selector: anchor.dataset.selector });
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
    try {
      localStorage.setItem(sheetKey, open ? "open" : "closed");
    } catch (_error) {
      // Not remembered across reloads; nothing else depends on it.
    }
  }

  try {
    if (PHONE.matches && localStorage.getItem(sheetKey) === "open") setSheet(true);
  } catch (_error) {
    // Storage can be unavailable (private mode); the sheet starts closed.
  }

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
      case "reload":
        reloadFrame(root.dataset.artifact + "?rev=" + Date.now());
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
  // artifact forwards the same shortcut); Escape closes the menu or card.
  window.addEventListener(
    "keydown",
    (event) => {
      const k = String(event.key || "").toLowerCase();
      if ((event.metaKey || event.ctrlKey) && !event.shiftKey && !event.altKey && k === "i") {
        event.preventDefault();
        if (!state.ended) setMode(state.mode === "annotate" ? "explore" : "annotate", true);
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

  panelScroll.scrollTop = panelScroll.scrollHeight;
  updateControls();
  connect();
  // The artifact may have loaded (and said "ready") before this script ran.
  toFrame("hello");
})();
