// RadioStrangler showcase UI: client-side router, API client that records which backend served
// each call, and the persistent live player.
//
// Data from the API is always inserted with textContent, never innerHTML: the page has a strict
// CSP, and escaping by construction keeps it that way.

(function () {
  "use strict";

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = text;
    return node;
  }

  // localStorage can throw (private mode, blocked storage); the page must work without it.
  const store = {
    get(key) { try { return localStorage.getItem(key); } catch { return null; } },
    set(key, value) { try { localStorage.setItem(key, value); } catch { /* ignore */ } },
  };

  // --------------------------------------------------------------- hero image
  // CSP forbids inline style attributes, but setting a custom property through the CSSOM is fine.
  const hero = document.body.dataset.hero;
  if (hero) {
    document.documentElement.style.setProperty("--hero-img", `url("${encodeURI(hero)}")`);
  }

  // --------------------------------------------------------------- canary identity
  // Canary routes pick a side per client. A random id per browser keeps that choice stable
  // across reloads, and "new identity" lets a visitor land on the other side.
  const ID_KEY = "radiostrangler.clientId";

  function randomId() {
    const bytes = new Uint8Array(4);
    crypto.getRandomValues(bytes);
    return "web-" + Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
  }

  let clientId = store.get(ID_KEY) || randomId();
  store.set(ID_KEY, clientId);
  $("#clientId").textContent = clientId;

  // --------------------------------------------------------------- API client + telemetry
  const MAX_LOG = 8;
  const recentCalls = [];
  const lastCallByRoute = new Map();

  async function api(path) {
    const started = performance.now();
    let res = null;
    let body = null;
    let error = null;
    try {
      res = await fetch(path, {
        headers: { "X-Client-ID": clientId, Accept: "application/json" },
        cache: "no-store",
      });
      body = await res.json().catch(() => null);
      if (!res.ok) error = (body && body.error) || `HTTP ${res.status}`;
    } catch {
      error = "network error";
    }

    const call = {
      path,
      status: res ? res.status : 0,
      servedBy: (res && res.headers.get("X-Served-By")) || "",
      mode: (res && res.headers.get("X-Route-Mode")) || "",
      requestId: (res && res.headers.get("X-Request-ID")) || "",
      ms: Math.round(performance.now() - started),
      at: new Date(),
    };
    record(call);

    if (error) throw new Error(error);
    return body;
  }

  function record(call) {
    recentCalls.unshift(call);
    recentCalls.length = Math.min(recentCalls.length, MAX_LOG);
    lastCallByRoute.set(call.path, call);
    updateBadges(call);
    renderHood();
  }

  function updateBadges(call) {
    for (const badge of $$(`.served[data-route="${call.path}"]`)) {
      if (call.servedBy) {
        badge.className = `served ${call.servedBy}`;
        badge.textContent = `served by ${call.servedBy} · ${call.mode} · ${call.ms} ms`;
        badge.title = `Request ${call.requestId}`;
      } else {
        badge.className = "served error";
        badge.textContent = call.status ? `error ${call.status}` : "offline";
      }
    }
  }

  function renderHood() {
    const table = $("#routeTable");
    table.replaceChildren();
    const routes = Array.from(lastCallByRoute.values()).sort((a, b) => a.path.localeCompare(b.path));
    for (const c of routes) {
      const tr = el("tr");
      const route = el("td");
      route.append(el("code", null, c.path));
      tr.append(route, el("td", `mode ${c.mode}`, c.mode || "—"), el("td", null, c.servedBy || "—"), el("td", null, `${c.ms} ms`));
      table.append(tr);
    }

    const log = $("#callLog");
    log.replaceChildren();
    for (const c of recentCalls) {
      const li = el("li");
      li.append(
        el("span", "t", c.at.toLocaleTimeString([], { hour12: false })),
        el("span", null, `GET ${c.path}`),
        el("span", null, String(c.status || "ERR")),
        el("span", `by-${c.servedBy}`, c.servedBy || "—"),
        el("span", null, `${c.ms}ms`),
        el("span", "id", c.requestId),
      );
      log.append(li);
    }
  }

  // --------------------------------------------------------------- formatting
  const DAYS = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

  function duration(seconds) {
    const m = Math.floor(seconds / 60);
    const s = String(seconds % 60).padStart(2, "0");
    return `${m}:${s}`;
  }

  function ago(iso) {
    const secs = Math.max(0, (Date.now() - Date.parse(iso)) / 1000);
    if (!Number.isFinite(secs)) return "";
    if (secs < 60) return "just now";
    if (secs < 3600) return `${Math.floor(secs / 60)} min ago`;
    if (secs < 86400) return `${Math.floor(secs / 3600)} h ago`;
    return `${Math.floor(secs / 86400)} d ago`;
  }

  // The legacy contract says tags is always an array. When the Go side breaks it (the injected
  // demo bug), show the violation instead of crashing like a naive `tags.map` client would.
  function renderTags(container, tags) {
    container.replaceChildren();
    if (!Array.isArray(tags)) {
      const chip = el("span", "tag violation", `⚠ tags: ${JSON.stringify(tags)}`);
      chip.title = "Contract violation: the legacy API returns [] here. A client calling tags.map() would crash.";
      container.append(chip);
      return;
    }
    if (tags.length === 0) {
      container.append(el("span", "tag empty", "no tags"));
      return;
    }
    for (const t of tags) container.append(el("span", "tag", t));
  }

  function showError(container, message) {
    const box = el("div", "error-box");
    box.append(el("strong", null, "Could not load this section. "), el("span", "muted", message));
    container.replaceChildren(box);
  }

  // --------------------------------------------------------------- pages
  async function loadNowPlaying() {
    const root = $("#nowPlaying");
    const title = $(".now-title", root);
    const meta = $(".now-meta", root);
    try {
      const data = await api("/api/now-playing");
      title.classList.remove("skeleton");
      if (!data || !data.track) {
        title.textContent = "Nothing on the catalog yet";
        meta.textContent = "";
        $(".tags", root).replaceChildren();
        return;
      }
      const t = data.track;
      title.textContent = t.title;
      meta.textContent = [t.artist, t.album || "single", duration(t.duration_seconds), data.started_at && `started ${ago(data.started_at)}`]
        .filter(Boolean)
        .join(" · ");
      renderTags($(".tags", root), t.tags);
    } catch (err) {
      title.textContent = "Catalog unavailable";
      meta.textContent = err.message;
    }
  }

  async function loadPrograms() {
    const grid = $("#programs");
    try {
      const { data } = await api("/api/programs");
      grid.replaceChildren();
      if (!data.length) grid.append(el("p", "muted", "No programs yet."));
      for (const p of data) {
        const card = el("article", "card");
        card.append(
          el("span", "day", DAYS[p.weekday] || `Day ${p.weekday}`),
          el("h2", null, p.name),
          el("p", "host", `with ${p.host}`),
          el("p", "schedule", `🕒 ${p.start_time} – ${p.end_time}`),
        );
        grid.append(card);
      }
    } catch (err) {
      showError(grid, err.message);
    }
  }

  async function loadTracks() {
    const list = $("#tracks");
    try {
      const { data } = await api("/api/tracks");
      list.replaceChildren();
      if (!data.length) list.append(el("li", "muted", "No tracks yet."));
      for (const t of data) {
        const li = el("li", "track");
        const info = el("div");
        const tags = el("div", "tags");
        renderTags(tags, t.tags);
        info.append(el("p", "title", t.title), el("p", "meta", `${t.artist} · ${t.album || "single"}`), tags);
        li.append(el("span", "num", String(t.id)), info, el("span", "dur", duration(t.duration_seconds)));
        list.append(li);
      }
    } catch (err) {
      showError(list, err.message);
    }
  }

  const loaders = { "/": loadNowPlaying, "/programs": loadPrograms, "/tracks": loadTracks };

  // --------------------------------------------------------------- router
  // Navigation never reloads the document, so the <audio> element keeps playing.
  function show(path) {
    if (!(path in loaders)) path = "/";
    for (const page of $$("[data-page]")) page.hidden = page.dataset.page !== path;
    for (const a of $$(".nav a")) {
      if (a.getAttribute("href") === path) a.setAttribute("aria-current", "page");
      else a.removeAttribute("aria-current");
    }
    loaders[path]();
  }

  document.addEventListener("click", (e) => {
    const link = e.target.closest("a[data-link]");
    if (!link || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    const path = link.getAttribute("href");
    if (path !== location.pathname) history.pushState(null, "", path);
    show(path);
    window.scrollTo({ top: 0 });
  });
  window.addEventListener("popstate", () => show(location.pathname));

  // The now-playing card refreshes while it is visible, so mode changes made through the admin
  // API show up without a reload.
  setInterval(() => {
    if (document.visibilityState === "visible" && location.pathname === "/") loadNowPlaying();
  }, 20000);

  $("#newIdentity").addEventListener("click", () => {
    clientId = randomId();
    store.set(ID_KEY, clientId);
    $("#clientId").textContent = clientId;
    show(location.pathname);
  });

  show(location.pathname);

  // --------------------------------------------------------------- player
  const audio = $("#radio");
  const playBtn = $("#playBtn");
  const vol = $("#vol");
  const status = $("#status");
  const dock = $("#dock");

  function setStatus(text, live) {
    status.textContent = text;
    dock.classList.toggle("is-live", !!live);
    document.body.classList.toggle("playing", !!live);
  }

  const savedVol = parseFloat(store.get("radiostrangler.volume"));
  audio.volume = Number.isNaN(savedVol) ? 0.8 : savedVol;
  vol.value = audio.volume;
  vol.addEventListener("input", () => {
    audio.volume = parseFloat(vol.value);
    store.set("radiostrangler.volume", vol.value);
  });

  // No audio.load() before play(): on chunked Icecast streams a forced reload can trap Chrome in a
  // waiting/pause loop. The native <source> fallback only applies at first load, so switching
  // MP3 → OGG later is done by hand.
  const mp3Src = audio.dataset.srcMp3 || "";
  const oggSrc = audio.dataset.srcOgg || "";
  let currentSrc = mp3Src;
  let bufferTimer = null;
  const clearBufferTimer = () => { clearTimeout(bufferTimer); bufferTimer = null; };

  function switchSource(next) {
    if (!next || next === currentSrc) return false;
    currentSrc = next;
    audio.src = next;
    audio.load();
    audio.play().catch(() => {});
    return true;
  }

  playBtn.addEventListener("click", () => {
    if (!audio.paused) {
      audio.pause();
      return;
    }
    setStatus("Connecting…", false);
    audio.play().catch((err) => {
      console.error("playback failed:", err);
      if (currentSrc === mp3Src && switchSource(oggSrc)) setStatus("Retrying…", false);
      else setStatus("Tap play to retry.", false);
    });
  });

  audio.addEventListener("playing", () => {
    clearBufferTimer();
    playBtn.classList.add("is-playing");
    setStatus("Live", true);
  });
  audio.addEventListener("pause", () => {
    clearBufferTimer();
    playBtn.classList.remove("is-playing");
    setStatus("Paused", false);
  });
  audio.addEventListener("waiting", () => {
    setStatus("Buffering…", false);
    clearBufferTimer();
    // Buffering for more than 10 s: try the other format once before giving up.
    bufferTimer = setTimeout(() => {
      if (audio.readyState >= 3) return;
      if (switchSource(currentSrc === mp3Src ? oggSrc : mp3Src)) setStatus("Switching stream…", false);
      else setStatus("Stream slow — tap play to retry.", false);
    }, 10000);
  });
  audio.addEventListener("error", () => {
    clearBufferTimer();
    setStatus("Stream error. Tap play to retry.", false);
  });

  // OS media controls (lock screen, headphones, car).
  if ("mediaSession" in navigator) {
    navigator.mediaSession.metadata = new MediaMetadata({
      title: "Live broadcast",
      artist: $(".dock-title").textContent.trim(),
      album: "RadioStrangler",
    });
    navigator.mediaSession.setActionHandler("play", () => playBtn.click());
    navigator.mediaSession.setActionHandler("pause", () => playBtn.click());
  }

  // --------------------------------------------------------------- dock panel
  const toggle = $("#dockToggle");
  const panel = $("#dockPanel");

  function setDockOpen(open) {
    dock.dataset.expanded = String(open);
    toggle.setAttribute("aria-expanded", String(open));
    panel.setAttribute("aria-hidden", String(!open));
  }

  toggle.addEventListener("click", () => setDockOpen(dock.dataset.expanded !== "true"));
  $("#dockClose").addEventListener("click", () => setDockOpen(false));
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") setDockOpen(false); });
  document.addEventListener("click", (e) => {
    if (e.target.closest('[data-trigger="dock-toggle"]')) {
      e.preventDefault();
      setDockOpen(true);
    }
  });
})();
