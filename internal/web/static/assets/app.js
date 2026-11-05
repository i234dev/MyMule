"use strict";
// MyMule web interface. Plain JavaScript on top of the REST API (/api/v1).
// Text from the network (file names...) is only ever set with
// textContent, never as HTML.

// ---------- Translations ----------
const STRINGS = {
  en: {
    "login.label": "API key",
    "login.hint": "It is the api_key line in mymule.conf, next to the program.",
    "login.submit": "Log in",
    "login.wrong": "Wrong key.",
    "logout": "Log out",
    "tab.downloads": "Downloads", "tab.search": "Search", "tab.sharing": "Sharing", "tab.media": "Movies & series",
    "media.placeholder": "Title of a movie or series",
    "media.searching": "Looking it up…",
    "media.none": "Nothing found. Try the original title or fewer words.",
    "media.back": "← Back to results",
    "media.movie": "Movie", "media.series": "Series",
    "media.season": "Season", "media.episode": "Episode", "media.all": "all",
    "media.pickseason": "Choose a season (and an episode, or leave it empty for the whole season).",
    "media.finding": "Searching eD2K for: {q}. This takes about 10 seconds.",
    "media.nofiles": "No files found for this one on eD2K right now.",
    "media.summary": "{n} files",
    "media.showhidden": "Show {n} hidden",
    "media.hidehidden": "Hide {n} hidden",
    "media.best": "recommended",
    "media.guess": "{l} (by title)",
    "media.subs": "subtitles",
    "media.why.no sources": "no sources",
    "media.why.sources outside eD2K (Kad/torrent/usenet)": "only found on Kad/torrent: may not download",
    "media.why.cam or screener copy": "cam or screener copy",
    "media.why.bigger than the maximum size": "too big",
    "tab.settings": "Settings",
    "set.library": "Movie and series library",
    "set.libhint": "Movies and episodes you download from the Movies & series tab can be renamed and moved to their own folders when they finish. Leave a folder empty to keep those files in the downloads folder.",
    "set.moviesdir": "Movies folder", "set.seriesdir": "Series folder",
    "set.dirhint": "A folder on the computer where MyMule runs. It is created if it doesn't exist.",
    "set.movietmpl": "Movie file name", "set.seriestmpl": "Episode file name",
    "set.example": "Example:", "set.custom": "Custom…",
    "set.tags": "Tags:", "set.tagshint": "\"/\" makes a folder; {season:2} adds a leading zero (01).",
    "set.titlelang": "Title in the file name", "set.titlelocal": "In your language", "set.titleorig": "Original title",
    "set.share": "Share the movie and series folders (with their subfolders)",
    "set.save": "Save", "set.saved": "Saved.",
    "add.placeholder": "Paste ed2k:// links here",
    "add.submit": "Add",
    "add.done": "Added: {n}",
    "dl.clear": "Clear finished",
    "dl.summary": "{n} downloads · {active} active",
    "dl.empty": "No downloads. Search for something or paste an ed2k:// link.",
    "dl.pause": "Pause", "dl.resume": "Resume", "dl.cancel": "Cancel", "dl.remove": "Remove",
    "state.downloading": "downloading", "state.queued": "queued", "state.stalled": "waiting for sources",
    "state.paused": "paused", "state.finished": "finished",
    "meta.sources": "{n} sources", "meta.from": "from {n}", "meta.queue": "queued at {n}", "meta.rank": "best place #{n}",
    "meta.eta": "{t} left",
    "confirm.cancel": "Cancel this download? The partial data is deleted.",
    "confirm.remove": "Remove this finished download from the list? The file stays on disk.",
    "confirm.no": "Keep it",
    "search.placeholder": "What are you looking for?",
    "search.type": "Type", "search.min": "Min. MB", "search.max": "Max. MB", "search.ext": "Extension",
    "search.submit": "Search",
    "search.running": "Searching all servers… this takes about 10 seconds.",
    "search.none": "Nothing found. Try other words or fewer filters.",
    "type.any": "Any", "type.video": "Video", "type.audio": "Audio", "type.doc": "Documents",
    "type.program": "Programs", "type.archive": "Archives", "type.cdimage": "CD/DVD images", "type.image": "Images",
    "results.summary": "{n} results",
    "results.filter": "Filter results",
    "results.get": "Download selected",
    "results.getn": "Download selected ({n})",
    "results.have": "in downloads",
    "results.download": "Download",
    "col.name": "Name", "col.size": "Size", "col.sources": "Sources",
    "share.files": "Shared files", "share.upload": "Upload", "share.slots": "Upload slots",
    "share.uploading": "Uploading now",
    "share.partial": "{n} partial", "share.hashing": "hashing {f} ({p}%)", "share.tohash": "{n} to hash",
    "share.total": "{s} sent in total", "share.limit": "limit {s}/s", "share.nolimit": "no limit",
    "share.queue": "{n} waiting in queue", "share.none": "Nobody is downloading from you right now.",
    "share.off": "Sharing is off (sharing = false in mymule.conf).",
    "server.none": "not connected", "server.high": "HighID", "server.low": "LowID",
    "server.lowhint": "LowID: other users can't connect to you. Forward the port in your router.",
    "err.network": "Can't reach MyMule. Is it running?",
  },
  es: {
    "login.label": "Clave de la API",
    "login.hint": "Es la línea api_key de mymule.conf, junto al programa.",
    "login.submit": "Entrar",
    "login.wrong": "Clave incorrecta.",
    "logout": "Salir",
    "tab.downloads": "Descargas", "tab.search": "Buscar", "tab.sharing": "Compartir", "tab.media": "Películas y series",
    "media.placeholder": "Título de una película o serie",
    "media.searching": "Buscando…",
    "media.none": "No se ha encontrado nada. Prueba con el título original o con menos palabras.",
    "media.back": "← Volver a los resultados",
    "media.movie": "Película", "media.series": "Serie",
    "media.season": "Temporada", "media.episode": "Episodio", "media.all": "todos",
    "media.pickseason": "Elige una temporada (y un episodio, o déjalo vacío para la temporada entera).",
    "media.finding": "Buscando en eD2K: {q}. Tarda unos 10 segundos.",
    "media.nofiles": "Ahora mismo no hay ficheros de esto en eD2K.",
    "media.summary": "{n} ficheros",
    "media.showhidden": "Mostrar {n} ocultos",
    "media.hidehidden": "Ocultar {n} ocultos",
    "media.best": "recomendado",
    "media.guess": "{l} (por el título)",
    "media.subs": "subtítulos",
    "media.why.no sources": "sin fuentes",
    "media.why.sources outside eD2K (Kad/torrent/usenet)": "solo está en Kad/torrent: puede que no baje",
    "media.why.cam or screener copy": "copia de cine (CAM) o screener",
    "media.why.bigger than the maximum size": "demasiado grande",
    "tab.settings": "Ajustes",
    "set.library": "Biblioteca de películas y series",
    "set.libhint": "Las películas y episodios que descargues desde la pestaña Películas y series se pueden renombrar y mover a sus propias carpetas al terminar. Deja una carpeta vacía para que se queden en la carpeta de descargas.",
    "set.moviesdir": "Carpeta de películas", "set.seriesdir": "Carpeta de series",
    "set.dirhint": "Una carpeta del equipo donde funciona MyMule. Se crea si no existe.",
    "set.movietmpl": "Nombre de las películas", "set.seriestmpl": "Nombre de los episodios",
    "set.example": "Ejemplo:", "set.custom": "Personalizada…",
    "set.tags": "Etiquetas:", "set.tagshint": "\"/\" crea una carpeta; {season:2} añade un cero delante (01).",
    "set.titlelang": "Título en el nombre del fichero", "set.titlelocal": "En tu idioma", "set.titleorig": "Título original",
    "set.share": "Compartir las carpetas de películas y series (con sus subcarpetas)",
    "set.save": "Guardar", "set.saved": "Guardado.",
    "add.placeholder": "Pega aquí enlaces ed2k://",
    "add.submit": "Añadir",
    "add.done": "Añadidos: {n}",
    "dl.clear": "Limpiar terminadas",
    "dl.summary": "{n} descargas · {active} activas",
    "dl.empty": "No hay descargas. Busca algo o pega un enlace ed2k://.",
    "dl.pause": "Pausar", "dl.resume": "Reanudar", "dl.cancel": "Cancelar", "dl.remove": "Quitar",
    "state.downloading": "descargando", "state.queued": "en cola", "state.stalled": "esperando fuentes",
    "state.paused": "en pausa", "state.finished": "terminada",
    "meta.sources": "{n} fuentes", "meta.from": "de {n}", "meta.queue": "en cola en {n}", "meta.rank": "mejor puesto #{n}",
    "meta.eta": "quedan {t}",
    "confirm.cancel": "¿Cancelar esta descarga? Se borra lo descargado.",
    "confirm.remove": "¿Quitar esta descarga terminada de la lista? El fichero se queda en el disco.",
    "confirm.no": "No",
    "search.placeholder": "¿Qué buscas?",
    "search.type": "Tipo", "search.min": "Mín. MB", "search.max": "Máx. MB", "search.ext": "Extensión",
    "search.submit": "Buscar",
    "search.running": "Buscando en todos los servidores… tarda unos 10 segundos.",
    "search.none": "No se ha encontrado nada. Prueba con otras palabras o menos filtros.",
    "type.any": "Cualquiera", "type.video": "Vídeo", "type.audio": "Audio", "type.doc": "Documentos",
    "type.program": "Programas", "type.archive": "Comprimidos", "type.cdimage": "Imágenes de CD/DVD", "type.image": "Imágenes",
    "results.summary": "{n} resultados",
    "results.filter": "Filtrar resultados",
    "results.get": "Descargar seleccionados",
    "results.getn": "Descargar seleccionados ({n})",
    "results.have": "en descargas",
    "results.download": "Descargar",
    "col.name": "Nombre", "col.size": "Tamaño", "col.sources": "Fuentes",
    "share.files": "Ficheros compartidos", "share.upload": "Subida", "share.slots": "Huecos de subida",
    "share.uploading": "Subiendo ahora",
    "share.partial": "{n} a medias", "share.hashing": "calculando hash de {f} ({p}%)", "share.tohash": "{n} pendientes de hash",
    "share.total": "{s} enviados en total", "share.limit": "límite {s}/s", "share.nolimit": "sin límite",
    "share.queue": "{n} esperando en la cola", "share.none": "Ahora mismo nadie te está descargando.",
    "share.off": "Compartir está desactivado (sharing = false en mymule.conf).",
    "server.none": "sin conexión", "server.high": "HighID", "server.low": "LowID",
    "server.lowhint": "LowID: los demás no pueden conectar contigo. Abre el puerto en el router.",
    "err.network": "No se puede conectar con MyMule. ¿Está en marcha?",
  },
};
const LANG = (navigator.languages || [navigator.language || "en"])
  .map(l => l.slice(0, 2).toLowerCase()).find(l => STRINGS[l]) || "en";
document.documentElement.lang = LANG;
// Titles of movies and series are asked for in the browser's language,
// even when the interface itself isn't translated to it.
// The full tag (es-ES, es-MX...) gives each country's titles.
const MEDIA_LANG = ((navigator.languages || [navigator.language || "en"])[0] || "en").slice(0, 10);

function t(key, vars) {
  let s = STRINGS[LANG][key] ?? STRINGS.en[key] ?? key;
  for (const [k, v] of Object.entries(vars || {})) s = s.replaceAll("{" + k + "}", v);
  return s;
}

function translatePage() {
  document.querySelectorAll("[data-t]").forEach(n => { n.textContent = t(n.dataset.t); });
  document.querySelectorAll("[data-tp]").forEach(n => { n.placeholder = t(n.dataset.tp); });
}

// ---------- Helpers ----------
const $ = sel => document.querySelector(sel);

// el builds an element; children are nodes or strings (set as text).
function el(tag, attrs, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") n.className = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else if (k === "text") n.textContent = v;
    else if (k === "style") Object.assign(n.style, v);
    else n.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    n.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return n;
}

const UNITS = ["B", "KB", "MB", "GB", "TB"];
function size(b) {
  b = Number(b) || 0;
  let i = 0;
  while (b >= 1024 && i < UNITS.length - 1) { b /= 1024; i++; }
  return (i === 0 ? Math.round(b) : b.toFixed(b < 10 ? 2 : b < 100 ? 1 : 0)) + " " + UNITS[i];
}
const speed = b => size(b) + "/s";
function duration(s) {
  if (s < 0) return "?";
  const d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60);
  if (d) return d + "d " + h + "h";
  if (h) return h + "h " + m + "m";
  if (m) return m + "m";
  return Math.max(1, Math.floor(s)) + "s";
}

class AuthError extends Error {}

async function api(method, path, body) {
  const opts = { method, headers: { "X-MyMule-UI": "1" }, credentials: "same-origin" };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  let res;
  try {
    res = await fetch(path, opts);
  } catch {
    throw new Error(t("err.network"));
  }
  if (res.status === 401) { showLogin(); throw new AuthError("unauthorized"); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || (data.errors || []).join("; ") || res.statusText);
  return data;
}

function showMsg(node, text, kind) {
  node.textContent = text;
  node.className = "msg" + (kind ? " " + kind : "");
  node.hidden = !text;
}

// ---------- Login ----------
function showLogin() {
  stopPolling();
  $("#app").hidden = true;
  $("#login").hidden = false;
  $("#login-key").focus();
}

$("#login-form").addEventListener("submit", async e => {
  e.preventDefault();
  const err = $("#login-error");
  err.hidden = true;
  const res = await fetch("/web/login", {
    method: "POST", credentials: "same-origin",
    headers: { "X-MyMule-UI": "1", "Content-Type": "application/json" },
    body: JSON.stringify({ key: $("#login-key").value }),
  }).catch(() => null);
  if (!res || !res.ok) {
    err.textContent = res ? t("login.wrong") : t("err.network");
    err.hidden = false;
    return;
  }
  $("#login-key").value = "";
  start();
});

$("#logout").addEventListener("click", async () => {
  await fetch("/web/logout", { method: "POST", credentials: "same-origin" }).catch(() => {});
  status = null;
  showLogin();
});

// ---------- Tabs ----------
let currentTab = "downloads";
function selectTab(name) {
  currentTab = name;
  document.querySelectorAll(".tabs button").forEach(b => b.setAttribute("aria-selected", b.dataset.tab === name));
  document.querySelectorAll(".tab").forEach(s => { s.hidden = s.id !== "tab-" + name; });
  if (name === "search") $("#s-q").focus();
  if (name === "media" && $("#work").hidden) $("#m-q").focus();
  if (name === "settings" && !prefsLoaded) loadSettings();
  try { localStorage.setItem("mymule.tab", name); } catch {}
  render();
}
document.querySelectorAll(".tabs button").forEach(b => b.addEventListener("click", () => selectTab(b.dataset.tab)));

// ---------- Status polling ----------
let status = null, timer = null;

async function refresh() {
  try {
    status = await api("GET", "/api/v1/status");
    render();
  } catch (e) {
    if (!(e instanceof AuthError)) $("#chip-server").textContent = e.message;
  }
}
function startPolling() {
  stopPolling();
  timer = setInterval(() => { if (!document.hidden) refresh(); }, 2000);
}
function stopPolling() { clearInterval(timer); timer = null; }
document.addEventListener("visibilitychange", () => { if (!document.hidden && timer) refresh(); });

function render() {
  if (!status) return;
  renderHeader();
  if (currentTab === "downloads") renderDownloads();
  if (currentTab === "sharing") renderSharing();
  if (currentTab === "search") markHave();
  if (currentTab === "media") markHaveReleases();
}

function renderHeader() {
  const s = status.server || {};
  const chip = $("#chip-server");
  if (!s.connected) {
    chip.textContent = t("server.none");
    chip.className = "chip bad";
    chip.title = "";
  } else {
    chip.textContent = (s.name || s.address) + " · " + (s.high_id ? t("server.high") : t("server.low"));
    chip.className = "chip " + (s.high_id ? "ok" : "warn");
    chip.title = s.high_id ? s.address : t("server.lowhint");
  }
  $("#chip-down").textContent = "↓ " + speed(status.download_speed);
  $("#chip-up").textContent = "↑ " + speed(status.upload ? status.upload.speed : 0);
  $("#version").textContent = "MyMule " + status.version;
}

// ---------- Downloads ----------
function renderDownloads() {
  const list = $("#dl-list");
  const dls = status.downloads || [];
  const active = dls.filter(d => d.state !== "finished" && d.state !== "paused").length;
  $("#dl-summary").textContent = dls.length ? t("dl.summary", { n: dls.length, active }) : "";
  $("#dl-clear").hidden = !dls.some(d => d.state === "finished");
  if (!dls.length) {
    list.replaceChildren(el("div", { class: "card empty", text: t("dl.empty") }));
    return;
  }
  list.replaceChildren(...dls.map(downloadRow));
}

function downloadRow(d) {
  const pct = Math.floor(d.progress * 1000) / 10;
  const meta = [el("span", { text: size(d.done) + " / " + size(d.size) + " (" + pct + "%)" })];
  if (d.state !== "finished") {
    if (d.speed > 0) meta.push(el("span", { text: "↓ " + speed(d.speed) }));
    if (d.eta > 0) meta.push(el("span", { text: t("meta.eta", { t: duration(d.eta) }) }));
    let src = t("meta.sources", { n: d.sources });
    if (d.downloading_from) src += " · " + t("meta.from", { n: d.downloading_from });
    if (d.queued_at) src += " · " + t("meta.queue", { n: d.queued_at });
    if (d.best_queue_rank) src += " · " + t("meta.rank", { n: d.best_queue_rank });
    meta.push(el("span", { text: src }));
  }
  const actions = [];
  if (d.state === "paused") actions.push(el("button", { class: "icon", text: t("dl.resume"), onclick: () => act("POST", d, "resume") }));
  else if (d.state !== "finished") actions.push(el("button", { class: "icon", text: t("dl.pause"), onclick: () => act("POST", d, "pause") }));
  const finished = d.state === "finished";
  actions.push(el("button", {
    class: "icon", text: finished ? t("dl.remove") : t("dl.cancel"),
    onclick: () => confirmAction(finished ? t("confirm.remove") : t("confirm.cancel"), d.name,
      finished ? t("dl.remove") : t("dl.cancel"), () => act("DELETE", d)),
  }));
  return el("div", { class: "card dl " + d.state },
    el("div", { class: "dl-head" },
      el("span", { class: "dl-name", text: d.name, title: d.name }),
      el("span", { class: "badge " + d.state, text: t("state." + d.state) }),
      el("span", { class: "dl-actions" }, actions)),
    el("div", { class: "bar" }, el("span", { style: { width: Math.min(100, d.progress * 100) + "%" } })),
    el("div", { class: "dl-meta" }, meta));
}

async function act(method, d, verb) {
  try {
    await api(method, "/api/v1/downloads/" + d.hash + (verb ? "/" + verb : ""));
  } catch (e) {
    if (!(e instanceof AuthError)) alert(e.message);
  }
  refresh();
}

function confirmAction(text, name, okLabel, onOK) {
  const dlg = $("#confirm");
  $("#confirm-text").textContent = text;
  $("#confirm-name").textContent = name;
  $("#confirm-ok").textContent = okLabel;
  dlg.returnValue = "";
  dlg.onclose = () => { if (dlg.returnValue === "ok") onOK(); };
  dlg.showModal();
}

$("#dl-clear").addEventListener("click", async () => {
  try { await api("POST", "/api/v1/downloads/clear"); } catch (e) { if (!(e instanceof AuthError)) alert(e.message); }
  refresh();
});

$("#add-form").addEventListener("submit", async e => {
  e.preventDefault();
  const links = $("#add-links").value.match(/ed2k:\/\/\|file\|[^\s]+?\|\//gi) || [];
  const msg = $("#add-msg");
  if (!links.length) { showMsg(msg, "ed2k://|file|...|/", "bad"); return; }
  try {
    const r = await api("POST", "/api/v1/downloads", { links });
    showMsg(msg, t("add.done", { n: r.added.length }) + (r.errors ? " · " + r.errors.join("; ") : ""), r.errors ? "bad" : "ok");
    $("#add-links").value = "";
  } catch (err) {
    if (!(err instanceof AuthError)) showMsg(msg, err.message, "bad");
  }
  refresh();
});

// ---------- Search ----------
let results = [], selected = new Set(), sortKey = "complete", sortAsc = false, searching = false;

$("#search-form").addEventListener("submit", async e => {
  e.preventDefault();
  if (searching) return;
  const q = $("#s-q").value.trim();
  if (!q) return;
  const params = new URLSearchParams({ q, global: "true" });
  const type = $("#s-type").value;
  if (type) params.set("type", type);
  const mb = v => String(Math.round(Number(v) * 1024 * 1024));
  if ($("#s-min").value) params.set("min", mb($("#s-min").value));
  if ($("#s-max").value) params.set("max", mb($("#s-max").value));
  if ($("#s-ext").value.trim()) params.set("ext", $("#s-ext").value.trim().replace(/^\./, ""));

  const msg = $("#search-msg");
  msg.replaceChildren(el("span", { class: "spinner" }), t("search.running"));
  msg.className = "msg";
  msg.hidden = false;
  searching = true;
  $("#search-form button[type=submit]").disabled = true;
  try {
    const r = await api("GET", "/api/v1/search?" + params);
    results = r.results || [];
    selected.clear();
    $("#results-filter").value = "";
    msg.hidden = results.length > 0;
    if (!results.length) showMsg(msg, t("search.none"));
    renderResults();
  } catch (err) {
    if (!(err instanceof AuthError)) showMsg(msg, err.message, "bad");
  } finally {
    searching = false;
    $("#search-form button[type=submit]").disabled = false;
  }
});

function haveHashes() {
  return new Set((status && status.downloads || []).map(d => d.hash.toUpperCase()));
}

function renderResults() {
  const table = $("#results");
  const filter = $("#results-filter").value.trim().toLowerCase();
  const words = filter.split(/\s+/).filter(Boolean);
  const rows = results
    .filter(r => words.every(w => r.name.toLowerCase().includes(w)))
    .sort((a, b) => {
      const x = sortKey === "name" ? a.name.localeCompare(b.name) : (a[sortKey] - b[sortKey]) || (a.sources - b.sources);
      return sortAsc ? x : -x;
    });
  $("#results-bar").hidden = results.length === 0;
  table.hidden = results.length === 0;
  $("#results-summary").textContent = t("results.summary", { n: rows.length });
  const have = haveHashes();
  table.querySelector("tbody").replaceChildren(...rows.map(r => {
    const h = r.hash.toUpperCase();
    const box = el("input", { type: "checkbox", "aria-label": r.name, checked: selected.has(h) });
    box.addEventListener("change", () => { box.checked ? selected.add(h) : selected.delete(h); updateGetButton(); });
    return el("tr", { class: (r.sources ? "" : "dead") + (have.has(h) ? " have" : ""), "data-hash": h },
      el("td", { class: "check" }, box),
      el("td", { class: "name", text: r.name, "data-have": t("results.have") }),
      el("td", { class: "num", text: size(r.size) }),
      el("td", { class: "num", text: r.complete + (r.sources > r.complete ? " / " + r.sources : "") }),
      el("td", { class: "act" }, el("button", { class: "icon", text: t("results.download"), onclick: () => download([r]) })));
  }));
  table.querySelectorAll("th[data-sort]").forEach(th => {
    th.classList.toggle("sorted", th.dataset.sort === sortKey);
    th.classList.toggle("asc", th.dataset.sort === sortKey && sortAsc);
  });
  $("#results-all").checked = rows.length > 0 && rows.every(r => selected.has(r.hash.toUpperCase()));
  updateGetButton();
}

function markHave() {
  const have = haveHashes();
  document.querySelectorAll("#results tbody tr").forEach(tr => tr.classList.toggle("have", have.has(tr.dataset.hash)));
}

function updateGetButton() {
  const b = $("#results-get");
  b.disabled = selected.size === 0;
  b.textContent = selected.size ? t("results.getn", { n: selected.size }) : t("results.get");
}

document.querySelectorAll("#results th[data-sort]").forEach(th => th.addEventListener("click", () => {
  if (sortKey === th.dataset.sort) sortAsc = !sortAsc;
  else { sortKey = th.dataset.sort; sortAsc = sortKey === "name"; }
  renderResults();
}));
$("#results-filter").addEventListener("input", renderResults);
$("#results-all").addEventListener("change", e => {
  document.querySelectorAll("#results tbody tr").forEach(tr => {
    e.target.checked ? selected.add(tr.dataset.hash) : selected.delete(tr.dataset.hash);
  });
  renderResults();
});
$("#results-get").addEventListener("click", () => download(results.filter(r => selected.has(r.hash.toUpperCase()))));

// download adds files; msg is where to report (the search tab by default).
async function download(list, msg) {
  if (!list.length) return;
  const fromSearch = !msg;
  msg = msg || $("#search-msg");
  try {
    const r = await api("POST", "/api/v1/downloads", { links: list.map(x => x.link) });
    showMsg(msg, t("add.done", { n: r.added.length }) + (r.errors ? " · " + r.errors.join("; ") : ""), r.errors ? "bad" : "ok");
    list.forEach(x => selected.delete(x.hash.toUpperCase()));
  } catch (err) {
    if (!(err instanceof AuthError)) showMsg(msg, err.message, "bad");
  }
  await refresh();
  if (fromSearch) renderResults();
}

// ---------- Movies & series ----------
const LANG_NAMES = { es: "ES", "es-419": "LAT", en: "EN", fr: "FR", de: "DE", it: "IT", ca: "CA", pt: "PT" };
let works = [], currentWork = null, releases = [], showHidden = false, findingReleases = false;

$("#media-form").addEventListener("submit", async e => {
  e.preventDefault();
  const q = $("#m-q").value.trim();
  if (!q) return;
  const msg = $("#media-msg");
  msg.replaceChildren(el("span", { class: "spinner" }), t("media.searching"));
  msg.className = "msg";
  msg.hidden = false;
  $("#work").hidden = true;
  $("#works").hidden = false;
  currentWork = null;
  try {
    const r = await api("GET", "/api/v1/media/search?" + new URLSearchParams({ q, lang: MEDIA_LANG }));
    works = r.works || [];
    msg.hidden = works.length > 0;
    if (!works.length) showMsg(msg, t("media.none"));
    renderWorks();
  } catch (err) {
    if (!(err instanceof AuthError)) showMsg(msg, err.message, "bad");
  }
});

function workSub(w) {
  const parts = [t(w.kind === "series" ? "media.series" : "media.movie")];
  if (w.year) parts.push(String(w.year));
  if (w.original_title && w.original_title.toLowerCase() !== w.title.toLowerCase()) parts.push(w.original_title);
  return parts.join(" · ");
}

// poster returns the image for a work, or a placeholder.
function poster(w, size) {
  if (!w.poster) return el("div", { class: "poster none", "aria-hidden": "true", text: w.kind === "series" ? "TV" : "🎬" });
  return el("img", { class: "poster", src: "/web/poster/" + size + w.poster, alt: "", loading: size === "w185" ? "lazy" : "eager" });
}

function renderWorks() {
  $("#works").replaceChildren(...works.map(w => el("button", { class: "card work", onclick: () => openWork(w) },
    poster(w, "w185"),
    el("span", { class: "work-text" },
      el("span", { class: "wt", text: w.title + (w.year ? " (" + w.year + ")" : "") }),
      el("span", { class: "wo", text: workSub(w) }),
      w.description ? el("span", { class: "wd", text: w.description }) : null))));
}

$("#work-back").addEventListener("click", () => {
  $("#work").hidden = true;
  $("#works").hidden = false;
  currentWork = null;
});

function openWork(w) {
  currentWork = w;
  releases = [];
  showHidden = false;
  $("#works").hidden = true;
  $("#media-msg").hidden = true;
  $("#work").hidden = false;
  $("#w-title").textContent = w.title + (w.year ? " (" + w.year + ")" : "");
  $("#w-sub").textContent = workSub(w);
  $("#w-desc").textContent = w.description || "";
  $("#w-poster").replaceChildren(poster(w, "w342"));
  $("#releases").replaceChildren();
  $("#rel-bar").hidden = true;
  const series = w.kind === "series";
  $("#episode-form").hidden = !series;
  if (series) {
    $("#w-season").value = "";
    $("#w-episode").value = "";
    showMsg($("#rel-msg"), t("media.pickseason"));
    $("#w-season").focus();
  } else {
    findReleases();
  }
}

$("#episode-form").addEventListener("submit", e => { e.preventDefault(); findReleases(); });

async function findReleases() {
  if (!currentWork || findingReleases) return;
  const w = currentWork;
  const params = new URLSearchParams({ lang: MEDIA_LANG });
  if (w.kind === "series") {
    if ($("#w-season").value) params.set("season", $("#w-season").value);
    if ($("#w-episode").value) params.set("episode", $("#w-episode").value);
  }
  const titles = [w.title, w.original_title].filter((x, i, a) =>
    x && a.findIndex(y => y && y.toLowerCase() === x.toLowerCase()) === i);
  const msg = $("#rel-msg");
  msg.replaceChildren(el("span", { class: "spinner" }), t("media.finding", { q: titles.join(" / ") }));
  msg.className = "msg";
  msg.hidden = false;
  $("#releases").replaceChildren();
  $("#rel-bar").hidden = true;
  findingReleases = true;
  try {
    const r = await api("GET", "/api/v1/media/" + encodeURIComponent(w.id) + "/releases?" + params);
    if (currentWork !== w) return; // the user went elsewhere meanwhile
    releases = r.releases || [];
    msg.hidden = releases.length > 0;
    if (!releases.length) showMsg(msg, t("media.nofiles"));
    renderReleases();
  } catch (err) {
    if (!(err instanceof AuthError)) showMsg(msg, err.message, "bad");
  } finally {
    findingReleases = false;
  }
}

$("#rel-hidden").addEventListener("click", () => { showHidden = !showHidden; renderReleases(); });

function releaseTags(r) {
  const tags = [];
  if (r.recommended) tags.push(el("span", { class: "tag best", text: "★ " + t("media.best") }));
  const langs = r.info.languages || [];
  for (const l of langs) tags.push(el("span", { class: "tag lang", text: LANG_NAMES[l] || l.toUpperCase() }));
  if (!langs.length && r.guessed_language) {
    const l = LANG_NAMES[r.guessed_language] || r.guessed_language.toUpperCase();
    tags.push(el("span", { class: "tag lang", text: t("media.guess", { l }) }));
  }
  if (r.info.subs) tags.push(el("span", { class: "tag", text: t("media.subs") }));
  if (r.info.resolution) tags.push(el("span", { class: "tag", text: r.info.resolution }));
  if (r.info.source) {
    const bad = r.info.source === "cam" || r.info.source === "screener";
    tags.push(el("span", { class: "tag" + (bad ? " warn" : ""), text: r.info.source }));
  }
  if (r.info.season) {
    const se = "S" + String(r.info.season).padStart(2, "0") + (r.info.episode ? "E" + String(r.info.episode).padStart(2, "0") : "");
    tags.push(el("span", { class: "tag", text: se }));
  }
  tags.push(el("span", { text: size(r.size) }));
  tags.push(el("span", { text: t("meta.sources", { n: r.complete + (r.sources > r.complete ? "/" + r.sources : "") }) }));
  if (r.hidden) tags.push(el("span", { class: "tag warn", text: t("media.why." + r.hidden_why) }));
  return tags;
}

function renderReleases() {
  const hidden = releases.filter(r => r.hidden).length;
  const shown = releases.filter(r => showHidden || !r.hidden);
  $("#rel-bar").hidden = releases.length === 0;
  $("#rel-summary").textContent = t("media.summary", { n: releases.length - hidden });
  const hb = $("#rel-hidden");
  hb.hidden = hidden === 0;
  hb.textContent = t(showHidden ? "media.hidehidden" : "media.showhidden", { n: hidden });
  const have = haveHashes();
  $("#releases").replaceChildren(...shown.map(r => {
    const inDl = have.has(r.hash.toUpperCase());
    return el("div", { class: "card rel" + (r.recommended ? " recommended" : "") + (r.hidden ? " hidden" : ""), "data-hash": r.hash.toUpperCase() },
      el("div", { class: "rel-head" },
        el("span", { class: "rel-name", text: r.name, title: r.name }),
        el("button", {
          class: inDl ? "icon" : "primary small", disabled: inDl,
          text: inDl ? t("results.have") : t("results.download"),
          onclick: () => downloadRelease(r),
        })),
      el("div", { class: "tags" }, releaseTags(r)));
  }));
}

// downloadRelease adds a file of the open movie or series, telling MyMule
// what it is so it can be renamed into the library when it finishes.
async function downloadRelease(r) {
  const w = currentWork, msg = $("#rel-msg");
  const body = { link: r.link, work_id: w.id, lang: MEDIA_LANG };
  if (w.kind === "series") {
    body.season = Number($("#w-season").value) || 0;
    body.episode = Number($("#w-episode").value) || 0;
  }
  try {
    const res = await api("POST", "/api/v1/media/download", body);
    showMsg(msg, t("add.done", { n: res.added.length }), "ok");
  } catch (err) {
    if (!(err instanceof AuthError)) showMsg(msg, err.message, "bad");
  }
  await refresh();
  renderReleases();
}

function markHaveReleases() {
  if (!releases.length) return;
  const have = haveHashes();
  document.querySelectorAll("#releases .rel").forEach(n => {
    const b = n.querySelector("button");
    if (b && have.has(n.dataset.hash) && !b.disabled) {
      b.disabled = true;
      b.className = "icon";
      b.textContent = t("results.have");
    }
  });
}

// ---------- Settings ----------
let presets = { movie: [], series: [] }, prefsLoaded = false, previewTimer = null;
const CUSTOM = "__custom__";

function fillPresets(sel, list, current) {
  sel.replaceChildren(...list.map(p => el("option", { value: p, text: p })),
    el("option", { value: CUSTOM, text: t("set.custom") }));
  sel.value = list.includes(current) ? current : CUSTOM;
}

async function loadSettings() {
  try {
    const r = await api("GET", "/api/v1/preferences");
    presets = r.presets;
    const l = r.preferences.library;
    $("#set-movies-dir").value = l.movies_dir || "";
    $("#set-series-dir").value = l.series_dir || "";
    $("#set-movie-tmpl").value = l.movie_template;
    $("#set-series-tmpl").value = l.series_template;
    fillPresets($("#set-movie-preset"), presets.movie, l.movie_template);
    fillPresets($("#set-series-preset"), presets.series, l.series_template);
    document.querySelectorAll('input[name="set-title"]').forEach(r => { r.checked = (r.value === "original") === !!l.original_titles; });
    $("#set-share").checked = !!l.share;
    prefsLoaded = true;
    preview();
  } catch (err) {
    if (!(err instanceof AuthError)) showMsg($("#lib-msg"), err.message, "bad");
  }
}

function libraryForm() {
  return {
    movies_dir: $("#set-movies-dir").value.trim(),
    series_dir: $("#set-series-dir").value.trim(),
    movie_template: $("#set-movie-tmpl").value.trim(),
    series_template: $("#set-series-tmpl").value.trim(),
    original_titles: document.querySelector('input[name="set-title"]:checked').value === "original",
    share: $("#set-share").checked,
  };
}

function preview() {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(async () => {
    try {
      const r = await api("POST", "/api/v1/preferences/preview", libraryForm());
      for (const k of ["movie", "series"]) {
        const n = $("#prev-" + k);
        n.textContent = r[k] || r[k + "_error"] || "";
        n.classList.toggle("error", !r[k]);
      }
    } catch { /* shown on save */ }
  }, 250);
}

for (const k of ["movie", "series"]) {
  $("#set-" + k + "-preset").addEventListener("change", e => {
    if (e.target.value !== CUSTOM) $("#set-" + k + "-tmpl").value = e.target.value;
    else $("#set-" + k + "-tmpl").focus();
    preview();
  });
  $("#set-" + k + "-tmpl").addEventListener("input", e => {
    const list = presets[k];
    $("#set-" + k + "-preset").value = list.includes(e.target.value.trim()) ? e.target.value.trim() : CUSTOM;
    preview();
  });
}
document.querySelectorAll('input[name="set-title"]').forEach(r => r.addEventListener("change", preview));

$("#lib-form").addEventListener("submit", async e => {
  e.preventDefault();
  const msg = $("#lib-msg");
  try {
    await api("PUT", "/api/v1/preferences", { library: libraryForm() });
    showMsg(msg, t("set.saved"), "ok");
  } catch (err) {
    if (!(err instanceof AuthError)) showMsg(msg, err.message, "bad");
  }
});

// ---------- Sharing ----------
function renderSharing() {
  const sh = status.shared, up = status.upload;
  $("#slots-title").hidden = !sh || !up;
  if (!sh || !up) {
    $("#sh-files").textContent = "—";
    $("#sh-files-sub").textContent = t("share.off");
    $("#sh-speed").textContent = $("#sh-slots").textContent = "—";
    $("#sh-speed-sub").textContent = $("#sh-slots-sub").textContent = "";
    $("#slots").replaceChildren();
    return;
  }
  $("#sh-files").textContent = sh.files + " · " + size(sh.bytes);
  const sub = [];
  if (sh.partial) sub.push(t("share.partial", { n: sh.partial }));
  if (sh.hashing) sub.push(t("share.hashing", { f: sh.hashing, p: sh.hashed_pct || 0 }));
  if (sh.to_hash) sub.push(t("share.tohash", { n: sh.to_hash }));
  $("#sh-files-sub").textContent = sub.join(" · ");
  $("#sh-speed").textContent = speed(up.speed);
  $("#sh-speed-sub").textContent = t("share.total", { s: size(up.total) }) + " · " +
    (up.limit_kb ? t("share.limit", { s: size(up.limit_kb * 1024) }) : t("share.nolimit"));
  const slots = up.slots || [];
  $("#sh-slots").textContent = slots.length + " / " + up.max_slots;
  $("#sh-slots-sub").textContent = t("share.queue", { n: up.queue });
  if (!slots.length) {
    $("#slots").replaceChildren(el("div", { class: "card empty", text: t("share.none") }));
    return;
  }
  $("#slots").replaceChildren(...slots.map(s => el("div", { class: "card dl" },
    el("div", { class: "dl-head" },
      el("span", { class: "dl-name", text: s.file }),
      el("span", { class: "badge downloading", text: speed(s.speed) })),
    el("div", { class: "dl-meta" },
      el("span", { text: s.name || "?" }),
      el("span", { text: s.addr }),
      el("span", { text: "↑ " + size(s.sent) })))));
}

// ---------- Terms of use ----------
const TERMS_TEXT = TERMS[LANG] || TERMS.en;

function termsAccepted() {
  try { return localStorage.getItem("mymule.terms") === TERMS_VERSION; } catch { return false; }
}

// showTerms opens the terms. First time (mustAccept): accept to go on,
// or decline to log out. Later, from the footer link, just to read them.
function showTerms(mustAccept) {
  const T = TERMS_TEXT, dlg = $("#terms");
  $("#terms-title").textContent = T.title;
  $("#terms-intro").textContent = T.intro;
  $("#terms-items").replaceChildren(...T.items.map(([head, body]) =>
    el("li", {}, el("strong", { text: head }), " " + body)));
  $("#terms-outro").textContent = T.outro;
  $("#terms-accept").textContent = mustAccept ? T.accept : T.close;
  $("#terms-decline").textContent = T.decline;
  $("#terms-decline").hidden = !mustAccept;
  dlg.returnValue = "";
  dlg.onclose = () => {
    if (!mustAccept) return;
    if (dlg.returnValue === "accept") {
      try { localStorage.setItem("mymule.terms", TERMS_VERSION); } catch {}
    } else {
      $("#logout").click();
    }
  };
  // Esc would close it without an answer: treat it as "not yet".
  dlg.oncancel = e => { if (mustAccept) e.preventDefault(); };
  dlg.showModal();
}

$("#terms-link").textContent = TERMS_TEXT.link;
$("#terms-link").addEventListener("click", e => { e.preventDefault(); showTerms(false); });

// ---------- Start ----------
async function start() {
  try {
    status = await api("GET", "/api/v1/status");
  } catch (e) {
    if (!(e instanceof AuthError)) {
      $("#login").hidden = false;
      const err = $("#login-error");
      err.textContent = e.message;
      err.hidden = false;
    }
    return;
  }
  $("#login").hidden = true;
  $("#app").hidden = false;
  let tab = "downloads";
  try { tab = localStorage.getItem("mymule.tab") || tab; } catch {}
  selectTab(["downloads", "media", "search", "sharing", "settings"].includes(tab) ? tab : "downloads");
  startPolling();
  if (!termsAccepted()) showTerms(true);
}

translatePage();
start();
