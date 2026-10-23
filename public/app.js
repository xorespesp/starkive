// The starkive dashboard page.
// This file draws the page and sends change requests.
// The server (`/api/*`) does the rest: saving, issuing IDs, checking input, handling conflicts and syncing.
//
// Repository details come from three places, and `view` merges them:
// the archive item (name, note, star lists), the snapshot from the last sync, and live details from GitHub.
//
// Design rules:
//   - The page is drawn only from `state`, so `render` is safe to call at any time
//   - Requests in a session go through `api`, and closing the session cancels them (`isCancelled`)
//   - Every dialog is made with `openModal`
//   - A value that depends on other values is set in one update function
//   - Work that can fail changes the page only after the server confirms success.
//     Removing an item is the one exception: it is hidden at once and comes back if the removal fails (`removeStarItem`)
//
// Security: text written by others goes in only with `textContent`, and GitHub links pass `safeUrl`.

const PAGE_SIZE = 60; // Items to draw each time the list grows. This avoids building thousands of DOM nodes at once
// Max characters for input fields (`maxLength`).
// They match the server limits (`core.MaxNoteLength`, `core.MaxStarListNameLength`, `core.MaxStarListDescriptionLength`).
// The server does the real check.
// These values only stop typing past the limit
const TEXT_LIMITS = { note: 2000, starListName: 100, starListDescription: 300 };
// Repositories to ask about in one live details request.
// GitHub GraphQL takes longer the more IDs it gets (100 IDs about 4.5 seconds, 25 IDs about 1.7 seconds, measured).
// So small batches sent in parallel finish sooner overall
const LIVE_META_BATCH = 25;
const LIVE_META_PARALLEL = 12; // Live details requests sent in parallel. This is for the deployed site (HTTP/2). With the local dev server (HTTP/1.1), the browser limits it to 6

// `state.selected` values for the special sidebar entries. Star list IDs are numbers, so they never clash with these strings
const ALL = ''; // Everything
const UNLISTED = ':unlisted'; // Items that are in no star list
const UNAVAILABLE = ':unavailable'; // Repositories that GitHub can no longer find

// All page state.
// `render` draws from this alone
const state = {
  allStarItems: [], // All archive items (`db/star-items.json` as is), including the ones being removed
  removingIds: new Set(), // IDs of items whose removal was sent and not answered yet
  // Archive items to show: `allStarItems` without the ones being removed.
  // The page reads and writes the archive only through this, so no reader has to leave them out on its own
  get starItems() {
    return this.removingIds.size ? this.allStarItems.filter((item) => !this.removingIds.has(item.id)) : this.allStarItems;
  },
  set starItems(items) {
    this.allStarItems = items;
  },
  starLists: [], // Star list definitions `{ id, name, description }`
  liveMeta: new Map(), // `node_id` -> live repository details from GitHub (in the GraphQL response shape)
  missing: new Set(), // `node_id`s that GitHub answered "not found" for (deleted, made private and so on)
  archivedMeta: new Map(), // Archive item ID -> the metadata snapshot left by sync (an entry of `db/repo-meta.json`)
  selected: ALL, // The sidebar entry chosen: `ALL`, `UNLISTED`, `UNAVAILABLE` or a star list ID
  query: '', // Search text. Only items that contain every space-separated word are shown
  language: '', // Language filter. An empty string means all languages
  sort: 'newest', // Sort order: `newest`, `oldest` (both by `starred_at`) or `name`
  shown: PAGE_SIZE, // Items to draw in the list. It grows when you scroll down or press "Load more"
  repo: '', // DB repository name (owner/repo). Comes with the sign-in response
  sync: null, // Sync status (`core.SyncStatus`: interval, workflow state, last scheduled run, running and last runs) from `fetchSyncStatus`. `null` if not received yet, or if it couldn't be read
  syncError: '', // Why the sync status couldn't be read (`fetchSyncStatus`). An empty string if it was read
  syncRequested: false, // `true` from pressing Sync now until the server answers. After that, `sync.active_run` shows the run
  noteDrafts: new Map(), // Item ID -> note being edited `{ draft, saving }`. Several items can be edited at once. It lives here, not in the DOM, so a redraw keeps the text
};

/**
 * Finds an element on the page by ID.
 *
 * @param {string} id The element's `id`
 * @returns {HTMLElement|null}
 */
const $ = (id) => document.getElementById(id);

// ---------------------------------------------------------------------------
// Server requests
// ---------------------------------------------------------------------------

/**
 * Error for a failed `/api/*` request.
 * It also holds the status code and the server's body, so the caller can tell cases apart.
 */
class ApiError extends Error {
  /**
   * @param {number} status HTTP status code
   * @param {string} message Text to show on the page (the server's `error`, or the status line if there is none)
   * @param {object} body The JSON error body from the server. `{}` if it wasn't JSON
   */
  constructor(status, message, body) {
    super(message);
    this.status = status;
    this.body = body;
  }
}

// Cancel handle (`AbortController`) of the open dashboard session.
// `null` on the sign-in screen.
// `api` puts its signal on every request except the sign-in API (`login-session`).
// So closing the session (reload, sign-out, session expiry) cancels those requests in flight at once.
// Async work that awaits `api` then stops at the cancel error, without its own "is this still the same session?" check
let sessionScope = null;

/**
 * Opens a new dashboard session.
 * All requests still running in the previous session are cancelled.
 *
 * @returns {AbortSignal} This session's cancel signal. Long-running loops check it to see whether the session closed
 */
function beginSession() {
  sessionScope?.abort();
  sessionScope = new AbortController();
  return sessionScope.signal;
}

/**
 * Closes the session and goes back to the sign-in screen.
 * It cancels requests in flight, closes open dialogs and clears the loaded data.
 *
 * @param {string} message Text to show in the status line of the sign-in screen
 * @param {{ error?: boolean }} [options] If `error` is `true`, the text is shown as an error and stays longer
 */
function endSession(message, { error = false } = {}) {
  sessionScope?.abort();
  sessionScope = null;
  closeAllModals();
  clearLoadedData();
  showLogin();
  setStatus(message, { error });
}

/**
 * Checks whether an error came from a request cancelled because the session closed.
 * Whoever closed the session already told the user, so such errors are not shown again.
 *
 * @param {unknown} err The caught error
 * @returns {boolean}
 */
const isCancelled = (err) => err?.name === 'AbortError';

/**
 * Calls `/api/*` on the same site and returns the JSON result.
 * The browser adds the session cookie, because it is a same-origin request.
 *
 * @param {string} path The path and query after `/api/` (e.g. `archive-star-items?id=3`)
 * @param {{ method?: string, body?: object, beforeSignIn?: boolean }} [options] HTTP method (GET by default) and the body to send as JSON.
 *   `beforeSignIn`: the request is sent before we know whether the user is signed in.
 *   A 401 then doesn't close the session, and is thrown to the caller
 * @returns {Promise<any>} The JSON from the server
 * @throws {ApiError} If the server answers with a failure. The message is the server's `error`
 * @throws {DOMException} `AbortError` if the request was cancelled because the session closed (`isCancelled`)
 *
 * NOTE: Requests other than the sign-in API (`login-session`) carry the current session's cancel signal.
 * A 401 on such a request means the session has ended.
 * So the session is closed here (`endSession`), wherever the call came from.
 * A 401 from the sign-in API means "not signed in yet", so the caller handles it.
 * The signal belongs to the session at the moment of the call.
 * So a loop that swallows errors must still rethrow cancel errors.
 * Otherwise, after the session closes, its next request goes out with no session (or with a new one).
 */
async function api(path, { method = 'GET', body, beforeSignIn = false } = {}) {
  const signal = path.startsWith('login-session') ? undefined : sessionScope?.signal; // The sign-in API is outside the session, so it gets no signal
  const res = await fetch(`/api/${path}`, {
    method,
    cache: 'no-store',
    headers: body ? { 'Content-Type': 'application/json' } : {},
    body: body && JSON.stringify(body),
    signal,
  });
  if (res.ok) {
    const data = await res.json();
    signal?.throwIfAborted(); // If the session closed while the response was read, drop the result
    return data;
  }

  let errorBody = {};
  try {
    errorBody = (await res.json()) ?? {};
  } catch {} // If the body isn't JSON (an error page from a proxy and so on), the status line becomes the message
  // A 401 on a request inside the session means the session has ended.
  // A 401 from the sign-in API is thrown to the caller as is
  if (res.status === 401 && signal && !beforeSignIn) {
    if (!signal.aborted) endSession(SESSION_EXPIRED, { error: true }); // Don't close a session that is already closed
    signal.throwIfAborted();
  }
  throw new ApiError(res.status, errorBody.error ?? `${res.status} ${res.statusText}`, errorBody);
}

const SESSION_EXPIRED = 'Session expired. Please sign in again'; // Text shown when the session expired and the page goes back to the sign-in screen

/**
 * Clears all loaded data and edit state.
 * After the session closes, no data from it stays behind the sign-in screen.
 */
function clearLoadedData() {
  Object.assign(state, { removingIds: new Set(), starItems: [], starLists: [], liveMeta: new Map(), missing: new Set(), archivedMeta: new Map(), noteDrafts: new Map(), sync: null, syncError: '', syncRequested: false });
  liveMetaLoader = newLiveMetaLoader(null);
}

/**
 * Requests the archive items, star list definitions and metadata snapshots in parallel.
 *
 * @param {boolean} [beforeSignIn] Whether the requests are sent before we know the user is signed in (the option of the same name in `api`)
 * @returns {Promise<[object[], object[], object[]]>} The contents of the three files
 */
function fetchArchiveFiles(beforeSignIn = false) {
  return Promise.all(['archive-star-items', 'archive-star-lists', 'archive-repo-meta'].map((path) => api(path, { beforeSignIn })));
}

/**
 * Requests the sync status (`sync-status`).
 *
 * @param {boolean} [beforeSignIn] Whether the request is sent before we know the user is signed in (the option of the same name in `api`)
 * @returns {Promise<{ sync: object|null, syncError: string, syncSettingWritesAtSend: number }>} The status (`core.SyncStatus`), or why it couldn't be read.
 *   A failed read doesn't throw: the dashboard works without the status, and `syncProblem` warns about it.
 *   `syncSettingWritesAtSend` is `syncSettingWrites` when the request was sent (`isSyncStatusOutdated`)
 * @throws {DOMException} `AbortError` if the session closed (`isCancelled`)
 */
async function fetchSyncStatus(beforeSignIn = false) {
  const syncSettingWritesAtSend = syncSettingWrites;
  try {
    return { sync: await api('sync-status', { beforeSignIn }), syncError: '', syncSettingWritesAtSend };
  } catch (err) {
    if (isCancelled(err)) throw err;
    return { sync: null, syncError: err.message, syncSettingWritesAtSend };
  }
}

// Number of sync setting changes that the server confirmed on this page.
// A status read sent before the last of them may hold the old setting (`isSyncStatusOutdated`)
let syncSettingWrites = 0;

/**
 * Reports whether the status read `result` was sent before a sync setting change that the server confirmed since.
 * Such a read may hold the old setting, and putting it in `state.sync` would undo the change on screen.
 *
 * @param {{ syncSettingWritesAtSend: number }} result A result of `fetchSyncStatus`
 * @returns {boolean}
 */
function isSyncStatusOutdated(result) {
  return result.syncSettingWritesAtSend !== syncSettingWrites;
}

/**
 * Receives the three archive files and puts them in `state`.
 * It clears the live repository details.
 * `requestLiveMeta` fetches them separately, only for items on screen.
 *
 * @param {AbortSignal} signal This session's cancel signal. It ties live details loading (`liveMetaLoader`) to this session
 * @param {Promise<[object[], object[], object[]]>} [archive] A request already sent (`fetchArchiveFiles`). If missing, it is sent now
 */
async function loadAll(signal, archive = fetchArchiveFiles()) {
  let archivedMeta;
  [state.starItems, state.starLists, archivedMeta] = await archive;
  state.archivedMeta = new Map(archivedMeta.map((m) => [m.id, m]));
  state.liveMeta = new Map();
  state.missing = new Set();
  liveMetaLoader = newLiveMetaLoader(signal);
}

/**
 * Fetches live details for one batch of repositories and puts them in `state.liveMeta`.
 * Ones that GitHub says it can't find go into `state.missing`.
 *
 * @param {string[]} nodeIds `node_id`s of the repositories to ask about (up to 100)
 */
async function fetchLiveMeta(nodeIds) {
  const { repos, missing } = await api('github-repo-meta', { method: 'POST', body: { ids: nodeIds } });
  for (const repo of repos) {
    state.liveMeta.set(repo.id, repo); // The GraphQL `id` is the `node_id`
    state.missing.delete(repo.id);
  }
  for (const id of missing) state.missing.add(id);
}

/**
 * Makes the live details loading state for one session.
 * Each session gets its own state.
 * `signal` is `null` while no session is open.
 *
 * @param {AbortSignal|null} signal This session's cancel signal
 * @returns {{ signal: AbortSignal|null, requested: Set<string>, queue: string[], workers: number, error: Error|null, reportErrors: boolean, idleWaiters: Function[] }}
 *   `requested`: `node_id`s ever put in the queue (failed ones are not asked again).
 *   `queue`: `node_id`s not sent yet.
 *   `workers`: number of workers running now.
 *   `error`: the error of the first failed batch.
 *   `reportErrors`: whether to show failures in the status line right away (turned on after the first screen is loaded).
 *   `idleWaiters`: `resolve` functions of callers waiting for all workers to finish
 */
function newLiveMetaLoader(signal) {
  return { signal, requested: new Set(), queue: [], workers: 0, error: null, reportErrors: false, idleWaiters: [] };
}

let liveMetaLoader = newLiveMetaLoader(null); // Live details loading state of the current session (`newLiveMetaLoader`)

/**
 * Queues live details for items not asked about yet.
 * If fewer than `LIVE_META_PARALLEL` workers are running, it starts more.
 * Call it on every draw with the items drawn on screen.
 * Then items that appear through scrolling, star list choice or search are filled in on their own.
 * Items already asked about are skipped, so calling it many times is safe.
 *
 * @param {object[]} items Archive items drawn on screen
 */
function requestLiveMeta(items) {
  const loader = liveMetaLoader;
  if (!loader.signal || loader.signal.aborted) return;
  for (const item of items) {
    if (item.node_id && !loader.requested.has(item.node_id)) {
      loader.requested.add(item.node_id);
      loader.queue.push(item.node_id);
    }
  }
  while (loader.workers < LIVE_META_PARALLEL && loader.queue.length) {
    loader.workers++;
    runLiveMetaWorker(loader);
  }
}

/**
 * Worker that takes `LIVE_META_BATCH` IDs at a time from the queue, fetches live details and redraws as they arrive.
 * It ends when the queue is empty.
 *
 * @param {object} loader Loading state of the session that started this worker (`newLiveMetaLoader`)
 *
 * NOTE: It stops when the session closes (checked before each batch, and on a cancel error).
 * Otherwise its next batch would go out with whatever session is open then, because `api` uses the signal of the moment it is called.
 * Any other failure drops only that batch (those repositories keep their snapshot or saved name).
 */
async function runLiveMetaWorker(loader) {
  while (loader.queue.length && !loader.signal.aborted) {
    const ids = loader.queue.splice(0, LIVE_META_BATCH);
    try {
      await fetchLiveMeta(ids);
      scheduleRender();
    } catch (err) {
      if (isCancelled(err)) break;
      if (!loader.error && loader.reportErrors) setStatus(`Could not load some repository details from GitHub: ${err.message}`, { error: true });
      loader.error ??= err;
    }
  }
  loader.workers--;
  if (loader.workers === 0) for (const resolve of loader.idleWaiters.splice(0)) resolve();
}

/**
 * Waits until all workers of the loading state `loader` finish.
 *
 * @param {object} loader Loading state (`newLiveMetaLoader`)
 * @returns {Promise<void>}
 */
function waitForLiveMetaIdle(loader) {
  return loader.workers === 0 ? Promise.resolve() : new Promise((resolve) => loader.idleWaiters.push(resolve));
}

let renderTimer = null; // Timer of the batched redraw. `null` if there is none

/**
 * Batches redraws into one per 250 ms.
 * Details arrive batch by batch, and drawing for each one would be wasteful.
 */
function scheduleRender() {
  if (renderTimer) return;
  renderTimer = setTimeout(() => {
    renderTimer = null;
    render();
  }, 250);
}

/**
 * Builds the repository details for the page.
 * It merges the archive item, the snapshot and the live details from GitHub.
 *
 * @param {object} item Archive item (one line of `db/star-items.json`)
 * @returns {object} Details for the page:
 *   `available` (whether GitHub can find it), `unavailableSince` (when it became unavailable, an empty string if unknown),
 *   `name` (current name), `previousName` (the name at archive time, if it differs), `url`, `description`, `language`, `languageColor`, `topics`,
 *   `stars`, `forks`, `pushedAt` (these three exist only in live details, `null` otherwise), `archived`
 *
 * NOTE: Values come from live details if present (they are right even if they differ from the snapshot).
 * Before live details arrive, or if they can't be fetched, the snapshot is used.
 * If neither exists, the saved name is used.
 * "Gone" means only that GitHub clearly answered it can't find the repository.
 * That is either a live check, or, before a live check, the record left by sync.
 * A repository whose request merely failed is not treated as gone.
 */
function view(item) {
  const live = state.liveMeta.get(item.node_id);
  const snap = state.archivedMeta.get(item.id);
  const gone = !live && (state.missing.has(item.node_id) || Boolean(snap?.unavailable_since));
  const name = live?.nameWithOwner ?? snap?.name ?? item.name;
  return {
    available: !gone,
    unavailableSince: gone ? snap?.unavailable_since ?? '' : '',
    name,
    previousName: name !== item.name ? item.name : '', // The name at archive time, if it differs
    url: live?.url ?? `https://github.com/${name}`, // Gone repositories keep a link too, so the user can check for themselves
    description: (live ? live.description : snap?.description) ?? '',
    language: (live ? live.primaryLanguage?.name : snap?.language) ?? '',
    languageColor: (live ? live.primaryLanguage?.color : snap?.language_color) ?? '',
    topics: (live ? live.repositoryTopics?.nodes.map((n) => n.topic.name) : snap?.topics) ?? [],
    stars: live?.stargazerCount ?? null,
    forks: live?.forkCount ?? null,
    pushedAt: live?.pushedAt ?? null,
    archived: (live ? live.isArchived : snap?.archived) ?? false,
  };
}

// ---------------------------------------------------------------------------
// Change requests.
// They update `state` with the server's result and redraw
// ---------------------------------------------------------------------------

/**
 * Adds a repository to the archive directly, without a Star, and puts it at the top of the list.
 * Once the server confirms the add, it goes into the list right away.
 *
 * @param {string} fullNameOrUrl owner/repo or a GitHub repository address. The server parses it
 * @param {number} starListId ID of the star list to add it to. 0 means no star list
 * @returns {Promise<object>} The added archive item
 * @throws {ApiError} Repository not found (404), already in the archive (409), star list not found (400) and so on
 *
 * NOTE: Live details are only for display, so they are not awaited.
 * `render` requests them for the new item (`requestLiveMeta`), and until then `view` uses the saved name.
 */
async function addRepo(fullNameOrUrl, starListId) {
  const item = await api('archive-star-items', { method: 'POST', body: { repo_full_name_or_url: fullNameOrUrl, star_list_id: starListId } });
  state.starItems = [item, ...state.starItems];
  render();
  return item;
}

/**
 * Changes an item's note or its star lists.
 * It updates `state` with the item the server returns.
 *
 * @param {number} id Archive item ID (GitHub repository ID)
 * @param {{ note?: string, star_list_ids?: number[] }} changes What to change. Missing keys stay as they are
 */
async function updateStarItem(id, changes) {
  const updated = await api(`archive-star-items?id=${id}`, { method: 'PATCH', body: changes });
  state.starItems = state.starItems.map((item) => (item.id === id ? updated : item));
  render();
}

/**
 * Removes an item from the archive.
 * The item leaves the page at once, and comes back to its place if the server refuses.
 *
 * @param {number} id Archive item ID
 *
 * NOTE: A 404 counts as removed, because only "not in the archive" gives it (GitHub's own 404 reaches us as 502).
 * So pressing it again after a removal whose answer was lost ends quietly.
 * A write to the archive while it is hidden (another edit, a reload) may drop it from `state.allStarItems`.
 * So on failure, the item is put back before the item that followed it, unless it is there already.
 */
async function removeStarItem(id) {
  const index = state.starItems.findIndex((item) => item.id === id);
  if (index < 0) return;
  const removed = state.starItems[index];
  const nextId = state.starItems[index + 1]?.id;
  const removingIds = state.removingIds; // `clearLoadedData` replaces it when the session closes
  removingIds.add(id);
  render();
  let failure = null;
  try {
    await api(`archive-star-items?id=${id}`, { method: 'DELETE' });
  } catch (err) {
    if (!(err instanceof ApiError && err.status === 404)) failure = err;
  }
  removingIds.delete(id);
  if (!failure) {
    state.starItems = state.starItems.filter((item) => item.id !== id);
    state.noteDrafts.delete(id); // Also drop the removed item's note draft
    return;
  }
  // A cancelled request may still have removed it on the server, so the next load decides
  if (!isCancelled(failure) && !state.starItems.some((item) => item.id === id)) {
    const items = [...state.starItems];
    const at = items.findIndex((item) => item.id === nextId);
    items.splice(at < 0 ? Math.min(index, items.length) : at, 0, removed);
    state.starItems = items;
  }
  render();
  throw failure;
}

/**
 * Creates a new star list.
 * The server issues the ID.
 *
 * @param {string} name Name. If the same name already exists (ignoring surrounding spaces and case), the server refuses with 409
 * @param {string} description Description. An empty string if there is none
 * @returns {Promise<object>} The new star list `{ id, name, description }`
 */
async function createStarList(name, description) {
  const starList = await api('archive-star-lists', { method: 'POST', body: { name, description } });
  state.starLists = [...state.starLists, starList];
  render();
  return starList;
}

/**
 * Changes a star list's name and description.
 *
 * @param {number} id Star list ID
 * @param {string} name New name
 * @param {string} description New description
 */
async function updateStarList(id, name, description) {
  const updated = await api(`archive-star-lists?id=${id}`, { method: 'PATCH', body: { name, description } });
  state.starLists = state.starLists.map((l) => (l.id === id ? updated : l));
  render();
}

/**
 * Deletes a star list.
 * Its repositories stay in the archive, and only their membership is removed.
 * The server deletes the definition and cleans up membership in one commit.
 * The page updates `state` to match that result.
 *
 * @param {number} id Star list ID
 */
async function deleteStarList(id) {
  await api(`archive-star-lists?id=${id}`, { method: 'DELETE' });
  state.starLists = state.starLists.filter((l) => l.id !== id);
  state.starItems = state.starItems.map((item) => (item.star_list_ids.includes(id) ? { ...item, star_list_ids: item.star_list_ids.filter((x) => x !== id) } : item));
  render();
}

/**
 * Finds a star list by ID.
 *
 * @param {number|string} id Star list ID. A special sidebar value (`ALL` and so on) finds nothing
 * @returns {object|undefined} The star list. `undefined` if there is none
 */
const starListById = (id) => state.starLists.find((l) => l.id === id);

/**
 * Returns only the item's star list IDs that still exist.
 * This lets the page ignore IDs of star lists deleted on another device.
 *
 * @param {object} item Archive item
 * @returns {number[]}
 */
function starListIdsOf(item) {
  return (item.star_list_ids ?? []).filter((id) => starListById(id));
}

// ---------------------------------------------------------------------------
// Rendering helpers
// ---------------------------------------------------------------------------

/**
 * Makes the progress indicator: three dots that rise in turn (`.spinner`).
 * It is decoration, so screen readers skip it.
 * The text next to it says what is going on.
 *
 * @returns {HTMLSpanElement}
 */
function spinner() {
  const box = el('span', { className: 'spinner' }, el('i'), el('i'), el('i'));
  box.setAttribute('aria-hidden', 'true');
  return box;
}

/**
 * Makes a DOM element from a tag, properties and children.
 *
 * @param {string} tag Tag name
 * @param {object} [props] Properties to set on the element as is (`className`, `textContent`, `onclick` and so on)
 * @param {...(Node|string|false|null|undefined)} children Children. Strings become text nodes, so they are not parsed as HTML.
 *   `false`, `null`, `undefined` and empty strings are skipped, so an optional element can be written as `cond && el(...)`
 * @returns {HTMLElement}
 */
function el(tag, props = {}, ...children) {
  const node = Object.assign(document.createElement(tag), props);
  node.append(...children.filter((c) => c !== false && c != null && c !== ''));
  return node;
}

/**
 * Returns the address as is, only if it is a GitHub address.
 * GitHub links and moves to GitHub use only addresses that passed this function.
 * This way, values set by others (like repository addresses) never send the user to another site.
 *
 * @param {unknown} url The address to check
 * @returns {string|null} The address if it starts with `https://github.com/`, otherwise `null`
 */
const safeUrl = (url) => (typeof url === 'string' && url.startsWith('https://github.com/') ? url : null);

// SVG paths (16x16) of the icons used on the page.
// Only the ones we use are taken from GitHub's Octicons (MIT license)
const ICONS = {
  pencil:
    'M11.013 1.427a1.75 1.75 0 0 1 2.474 0l1.086 1.086a1.75 1.75 0 0 1 0 2.474l-8.61 8.61c-.21.21-.47.364-.756.445l-3.251.93a.75.75 0 0 1-.927-.928l.929-3.25c.081-.286.235-.547.445-.758l8.61-8.61Zm.176 4.823L9.75 4.81l-6.286 6.287a.253.253 0 0 0-.064.108l-.558 1.953 1.953-.558a.253.253 0 0 0 .108-.064Zm1.238-3.763a.25.25 0 0 0-.354 0L10.811 3.75l1.439 1.44 1.263-1.263a.25.25 0 0 0 0-.354Z',
  trash:
    'M11 1.75V3h2.25a.75.75 0 0 1 0 1.5H2.75a.75.75 0 0 1 0-1.5H5V1.75C5 .784 5.784 0 6.75 0h2.5C10.216 0 11 .784 11 1.75ZM4.496 6.675l.66 6.6a.25.25 0 0 0 .249.225h5.19a.25.25 0 0 0 .249-.225l.66-6.6a.75.75 0 0 1 1.492.149l-.66 6.6A1.748 1.748 0 0 1 10.595 15h-5.19a1.75 1.75 0 0 1-1.741-1.575l-.66-6.6a.75.75 0 1 1 1.492-.15ZM6.5 1.75V3h3V1.75a.25.25 0 0 0-.25-.25h-2.5a.25.25 0 0 0-.25.25Z',
  list:
    'M5.75 2.5h8.5a.75.75 0 0 1 0 1.5h-8.5a.75.75 0 0 1 0-1.5Zm0 5h8.5a.75.75 0 0 1 0 1.5h-8.5a.75.75 0 0 1 0-1.5Zm0 5h8.5a.75.75 0 0 1 0 1.5h-8.5a.75.75 0 0 1 0-1.5ZM2 14a1 1 0 1 1 0-2 1 1 0 0 1 0 2Zm1-6a1 1 0 1 1-2 0 1 1 0 0 1 2 0ZM2 4a1 1 0 1 1 0-2 1 1 0 0 1 0 2Z',
  book: 'M0 1.75A.75.75 0 0 1 .75 1h4.253c1.227 0 2.317.59 3 1.501A3.743 3.743 0 0 1 11.006 1h4.245a.75.75 0 0 1 .75.75v10.5a.75.75 0 0 1-.75.75h-4.507a2.25 2.25 0 0 0-1.591.659l-.622.621a.75.75 0 0 1-1.06 0l-.622-.621A2.25 2.25 0 0 0 5.258 13H.75a.75.75 0 0 1-.75-.75Zm7.251 10.324.004-5.073-.002-2.253A2.25 2.25 0 0 0 5.003 2.5H1.5v9h3.757a3.75 3.75 0 0 1 1.994.574ZM8.755 4.75l-.004 7.322a3.752 3.752 0 0 1 1.992-.572H14.5v-9h-3.495a2.25 2.25 0 0 0-2.25 2.25Z',
  repo: 'M2 2.5A2.5 2.5 0 0 1 4.5 0h8.75a.75.75 0 0 1 .75.75v12.5a.75.75 0 0 1-.75.75h-2.5a.75.75 0 0 1 0-1.5h1.75v-2h-8a1 1 0 0 0-.714 1.7.75.75 0 1 1-1.072 1.05A2.495 2.495 0 0 1 2 11.5Zm10.5-1h-8a1 1 0 0 0-1 1v6.708A2.486 2.486 0 0 1 4.5 9h8ZM5 12.25a.25.25 0 0 1 .25-.25h3.5a.25.25 0 0 1 .25.25v3.25a.25.25 0 0 1-.4.2l-1.45-1.087a.249.249 0 0 0-.3 0L5.4 15.7a.25.25 0 0 1-.4-.2Z',
  linkExternal:
    'M3.75 2h3.5a.75.75 0 0 1 0 1.5h-3.5a.25.25 0 0 0-.25.25v8.5c0 .138.112.25.25.25h8.5a.25.25 0 0 0 .25-.25v-3.5a.75.75 0 0 1 1.5 0v3.5A1.75 1.75 0 0 1 12.25 14h-8.5A1.75 1.75 0 0 1 2 12.25v-8.5C2 2.784 2.784 2 3.75 2Zm6.854-1h4.146a.25.25 0 0 1 .25.25v4.146a.25.25 0 0 1-.427.177L13.03 4.03 9.28 7.78a.751.751 0 0 1-1.042-.018.751.751 0 0 1-.018-1.042l3.75-3.75-1.543-1.543A.25.25 0 0 1 10.604 1Z',
  sync:
    'M1.705 8.005a.75.75 0 0 1 .834.656 5.5 5.5 0 0 0 9.592 2.97l-1.204-1.204a.25.25 0 0 1 .177-.427h3.646a.25.25 0 0 1 .25.25v3.646a.25.25 0 0 1-.427.177l-1.38-1.38A7.002 7.002 0 0 1 1.05 8.84a.75.75 0 0 1 .656-.834ZM8 2.5a5.487 5.487 0 0 0-4.131 1.869l1.204 1.204A.25.25 0 0 1 4.896 6H1.25A.25.25 0 0 1 1 5.75V2.104a.25.25 0 0 1 .427-.177l1.38 1.38A7.002 7.002 0 0 1 14.95 7.16a.75.75 0 0 1-1.49.178A5.5 5.5 0 0 0 8 2.5Z',
  plus: 'M7.75 2a.75.75 0 0 1 .75.75V7h4.25a.75.75 0 0 1 0 1.5H8.5v4.25a.75.75 0 0 1-1.5 0V8.5H2.75a.75.75 0 0 1 0-1.5H7V2.75A.75.75 0 0 1 7.75 2Z',
  star:
    'M8 .25a.75.75 0 0 1 .673.418l1.882 3.815 4.21.612a.75.75 0 0 1 .416 1.279l-3.046 2.97.719 4.192a.751.751 0 0 1-1.088.791L8 12.347l-3.766 1.98a.75.75 0 0 1-1.088-.79l.72-4.194L.818 6.374a.75.75 0 0 1 .416-1.28l4.21-.611L7.327.668A.75.75 0 0 1 8 .25Zm0 2.445L6.615 5.5a.75.75 0 0 1-.564.41l-3.097.45 2.24 2.184a.75.75 0 0 1 .216.664l-.528 3.084 2.769-1.456a.75.75 0 0 1 .698 0l2.77 1.456-.53-3.084a.75.75 0 0 1 .216-.664l2.24-2.183-3.096-.45a.75.75 0 0 1-.564-.41L8 2.694Z',
  triangleDown: 'm4.427 7.427 3.396 3.396a.25.25 0 0 0 .354 0l3.396-3.396A.25.25 0 0 0 11.396 7H4.604a.25.25 0 0 0-.177.427Z',
  person:
    'M10.561 8.073a6.005 6.005 0 0 1 3.432 5.142.75.75 0 1 1-1.498.07 4.5 4.5 0 0 0-8.99 0 .75.75 0 0 1-1.498-.07 6.004 6.004 0 0 1 3.431-5.142 3.999 3.999 0 1 1 5.123 0ZM10.5 5a2.5 2.5 0 1 0-5 0 2.5 2.5 0 0 0 5 0Z',
  gear:
    'M8 0a8.2 8.2 0 0 1 .701.031C9.444.095 9.99.645 10.16 1.29l.288 1.107c.018.066.079.158.212.224.231.114.454.243.668.386.123.082.233.09.299.071l1.103-.303c.644-.176 1.392.021 1.82.63.27.385.506.792.704 1.218.315.675.111 1.422-.364 1.891l-.814.806c-.049.048-.098.147-.088.294.016.257.016.515 0 .772-.01.147.038.246.088.294l.814.806c.475.469.679 1.216.364 1.891a7.977 7.977 0 0 1-.704 1.217c-.428.61-1.176.807-1.82.63l-1.102-.302c-.067-.019-.177-.011-.3.071a5.909 5.909 0 0 1-.668.386c-.133.066-.194.158-.211.224l-.29 1.106c-.168.646-.715 1.196-1.458 1.26a8.006 8.006 0 0 1-1.402 0c-.743-.064-1.289-.614-1.458-1.26l-.289-1.106c-.018-.066-.079-.158-.212-.224a5.738 5.738 0 0 1-.668-.386c-.123-.082-.233-.09-.299-.071l-1.103.303c-.644.176-1.392-.021-1.82-.63a8.12 8.12 0 0 1-.704-1.218c-.315-.675-.111-1.422.363-1.891l.815-.806c.05-.048.098-.147.088-.294a6.214 6.214 0 0 1 0-.772c.01-.147-.038-.246-.088-.294l-.815-.806C.635 6.045.431 5.298.746 4.623a7.92 7.92 0 0 1 .704-1.217c.428-.61 1.176-.807 1.82-.63l1.102.302c.067.019.177.011.3-.071.214-.143.437-.272.668-.386.133-.066.194-.158.211-.224l.29-1.106C6.009.645 6.556.095 7.299.03 7.53.01 7.764 0 8 0Zm-.571 1.525c-.036.003-.108.036-.137.146l-.289 1.105c-.147.561-.549.967-.998 1.189-.173.086-.34.183-.5.29-.417.278-.97.423-1.529.27l-1.103-.303c-.109-.03-.175.016-.195.045-.22.312-.412.644-.573.99-.014.031-.021.11.059.19l.815.806c.411.406.562.957.53 1.456a4.709 4.709 0 0 0 0 .582c.032.499-.119 1.05-.53 1.456l-.815.806c-.081.08-.073.159-.059.19.162.346.353.677.573.989.02.03.085.076.195.046l1.102-.303c.56-.153 1.113-.008 1.53.27.161.107.328.204.501.29.447.222.85.629.997 1.189l.289 1.105c.029.109.101.143.137.146a6.6 6.6 0 0 0 1.142 0c.036-.003.108-.036.137-.146l.289-1.105c.147-.561.549-.967.998-1.189.173-.086.34-.183.5-.29.417-.278.97-.423 1.529-.27l1.103.303c.109.029.175-.016.195-.045.22-.313.411-.644.573-.99.014-.031.021-.11-.059-.19l-.815-.806c-.411-.406-.562-.957-.53-1.456a4.709 4.709 0 0 0 0-.582c-.032-.499.119-1.05.53-1.456l.815-.806c.081-.08.073-.159.059-.19a6.464 6.464 0 0 0-.573-.989c-.02-.03-.085-.076-.195-.046l-1.102.303c-.56.153-1.113.008-1.53-.27a4.44 4.44 0 0 0-.501-.29c-.447-.222-.85-.629-.997-1.189l-.289-1.105c-.029-.11-.101-.143-.137-.146a6.6 6.6 0 0 0-1.142 0ZM11 8a3 3 0 1 1-6 0 3 3 0 0 1 6 0ZM9.5 8a1.5 1.5 0 1 0-3.001.001A1.5 1.5 0 0 0 9.5 8Z',
  signOut:
    'M2 2.75C2 1.784 2.784 1 3.75 1h2.5a.75.75 0 0 1 0 1.5h-2.5a.25.25 0 0 0-.25.25v10.5c0 .138.112.25.25.25h2.5a.75.75 0 0 1 0 1.5h-2.5A1.75 1.75 0 0 1 2 13.25Zm10.44 4.5-1.97-1.97a.749.749 0 0 1 .326-1.275.749.749 0 0 1 .734.215l3.25 3.25a.75.75 0 0 1 0 1.06l-3.25 3.25a.749.749 0 0 1-1.275-.326.749.749 0 0 1 .215-.734l1.97-1.97H6.75a.75.75 0 0 1 0-1.5Z',
  fork:
    'M5 5.372v.878c0 .414.336.75.75.75h4.5a.75.75 0 0 0 .75-.75v-.878a2.25 2.25 0 1 1 1.5 0v.878a2.25 2.25 0 0 1-2.25 2.25h-1.5v2.128a2.251 2.251 0 1 1-1.5 0V8.5h-1.5A2.25 2.25 0 0 1 3.5 6.25v-.878a2.25 2.25 0 1 1 1.5 0ZM5 3.25a.75.75 0 1 0-1.5 0 .75.75 0 0 0 1.5 0Zm6.75.75a.75.75 0 1 0 0-1.5.75.75 0 0 0 0 1.5Zm-3 8.75a.75.75 0 1 0-1.5 0 .75.75 0 0 0 1.5 0Z',
};

/**
 * Makes an icon SVG element.
 *
 * @param {string} name A name in `ICONS`
 * @returns {SVGSVGElement}
 */
function icon(name) {
  const ns = 'http://www.w3.org/2000/svg'; // SVG elements are drawn only when made in this namespace (`createElementNS`, not `el`)
  const svg = document.createElementNS(ns, 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('width', '16');
  svg.setAttribute('height', '16');
  svg.setAttribute('aria-hidden', 'true');
  const path = document.createElementNS(ns, 'path');
  path.setAttribute('d', ICONS[name]);
  svg.append(path);
  return svg;
}

/**
 * Makes a button with only an icon.
 *
 * @param {string} name A name in `ICONS`
 * @param {string} label Button name. The button has no text, so this goes to screen readers (`aria-label`) and the hover tooltip (`title`)
 * @param {() => void} onclick What to do when pressed
 * @param {string} [className] Extra class (`danger` and so on)
 * @returns {HTMLButtonElement}
 */
function iconButton(name, label, onclick, className = '') {
  const button = el('button', { type: 'button', className: `icon-button ${className}`, title: label, onclick }, icon(name));
  button.setAttribute('aria-label', label);
  return button;
}

// Top bar menu open now (a `.menu` element).
// `null` if none.
// Only one menu is open at a time
let openMenu = null;

/** Closes the open top bar menu, if any. */
function closeMenu() {
  if (!openMenu) return;
  openMenu.querySelector('.menu-list').hidden = true;
  openMenu.querySelector('.menu-button').setAttribute('aria-expanded', 'false');
  openMenu = null;
}

/**
 * Attaches the open and close handling to a top bar menu.
 * The menu is a `.menu` element with a button (`.menu-button`) and a list (`.menu-list`) of items (`role="menuitem"`).
 *
 * @param {HTMLElement} menu The `.menu` element
 *
 * NOTE: The menu closes on a choice of an item, or when the focus moves out of it.
 * A choice gives the focus back to the button first.
 * So a dialog opened from the item gives the focus back to the button when it closes.
 * A menu opened from the keyboard focuses its first item, and the arrow keys, Home and End move between items.
 */
function bindMenu(menu) {
  const button = menu.querySelector('.menu-button');
  const list = menu.querySelector('.menu-list');
  const items = () => [...list.querySelectorAll('[role="menuitem"]')].filter((item) => !item.hidden);
  const open = () => {
    closeMenu();
    list.hidden = false;
    button.setAttribute('aria-expanded', 'true');
    openMenu = menu;
  };
  button.onclick = (ev) => {
    if (openMenu === menu) return closeMenu();
    open();
    if (ev.detail === 0) items()[0]?.focus(); // `detail` is 0 when Enter or Space pressed the button
  };
  for (const item of items()) {
    item.addEventListener('click', () => {
      closeMenu();
      button.focus();
    });
  }
  menu.addEventListener('focusout', (ev) => {
    if (openMenu === menu && ev.relatedTarget && !menu.contains(ev.relatedTarget)) closeMenu();
  });
  menu.addEventListener('keydown', (ev) => {
    if (openMenu !== menu) {
      if (ev.key !== 'ArrowDown' || ev.target !== button) return;
      ev.preventDefault();
      open();
      items()[0]?.focus();
      return;
    }
    const all = items();
    const index = all.indexOf(document.activeElement); // -1 while the focus is on the button
    const next = { ArrowDown: index + 1, ArrowUp: index < 0 ? -1 : index - 1, Home: 0, End: -1 }[ev.key];
    if (next === undefined) return;
    ev.preventDefault();
    all.at(next % all.length)?.focus(); // `at` counts a negative index from the end
  });
}

// Dialogs open now (values returned by `openModal`). When the session closes, `closeAllModals` closes them all
const openModals = new Set();

/**
 * Opens a dialog.
 * Every dialog is made with this function, so they all close the same way.
 *
 * @param {object} options
 * @param {HTMLElement} options.panel Element that holds all the dialog content. Padding goes on this element (`.dialog-panel`)
 * @param {string} [options.className] Extra class for the `dialog` element
 * @param {() => void} [options.onClose] What to do once when it closes
 * @returns {{ close: () => void }} Holds a function that closes the dialog. It does nothing if already closed
 *
 * NOTE: A dialog closes through `close()`, Esc, a backdrop click or `closeAllModals`.
 * Every way goes through the same handling, so `onClose` is called only once.
 * A backdrop click counts only when the press starts and ends on the backdrop.
 * This way, selecting text in an input by dragging and releasing outside doesn't close it.
 * The dialog closes at once, so the page takes focus and input right away.
 * Only the element is removed a moment later, so the fade-out in `style.css` can play.
 */
function openModal({ panel, className = '', onClose }) {
  closeMenu(); // A menu left open under the dialog would take the first Esc
  panel.classList.add('dialog-panel'); // Padding goes here, not on `dialog` (so a click on the padding isn't a backdrop click)
  const dialog = el('dialog', { className: `dialog ${className}`.trim() }, panel);
  const modal = {
    close: () => {
      if (!openModals.delete(modal)) return; // Already closed. Whatever the way, the code below runs only once
      dialog.close();
      setTimeout(() => dialog.remove(), DIALOG_CLOSE_MS);
      onClose?.();
    },
  };
  // A backdrop click counts only when both the press and the release are on the backdrop
  let pressedOnBackdrop = false;
  dialog.onpointerdown = (ev) => {
    pressedOnBackdrop = ev.target === dialog;
  };
  dialog.onclick = (ev) => {
    if (pressedOnBackdrop && ev.target === dialog) modal.close();
  };
  dialog.oncancel = (ev) => {
    ev.preventDefault(); // Stop the browser from closing it directly, and use the same close handling
    modal.close();
  };
  openModals.add(modal);
  document.body.append(dialog);
  dialog.showModal();
  return modal;
}

const DIALOG_CLOSE_MS = 200; // Time a closed dialog stays in the page before it is removed (milliseconds). Longer than the fade-out in `style.css`

/** Closes all open dialogs. */
function closeAllModals() {
  for (const modal of [...openModals]) modal.close(); // Closing removes it from `openModals`, so loop over a copy
}

/**
 * Shows a dialog with a title, content, and confirm and cancel buttons.
 *
 * @param {object} options
 * @param {string} options.title Title
 * @param {Node[]} [options.body] Content under the title
 * @param {string} options.confirmLabel Confirm button text
 * @param {boolean} [options.danger] `true` for work that can't be undone.
 *   The confirm button turns red, and the first focus goes to cancel, so a stray Enter doesn't run it
 * @param {() => Promise<void>|void} [options.onConfirm] What to do on confirm. If it throws, the message is shown and the dialog stays open
 * @param {() => boolean} [options.canConfirm] Whether the confirm button can be pressed with what is in the form now.
 *   Checked when the dialog opens and on every input. If missing, it can always be pressed
 * @returns {Promise<boolean>} `true` if closed by confirming, `false` if closed any other way
 *
 * NOTE: The browser's own `confirm()` and `prompt()` are blocked in some in-app browsers, and look different everywhere.
 * So we make our own.
 */
function showDialog({ title, body = [], confirmLabel, danger = false, onConfirm, canConfirm = () => true }) {
  return new Promise((resolve) => {
    const cancel = el('button', { type: 'button', className: 'ghost', textContent: 'Cancel' });
    const confirm = el('button', { type: 'submit', className: danger ? 'danger-solid' : '', textContent: confirmLabel });
    const error = el('p', { className: 'dialog-error', hidden: true });
    const form = el('form', {}, el('h2', { textContent: title }), ...body, error, el('div', { className: 'actions' }, cancel, confirm));
    let confirmed = false;
    const modal = openModal({ panel: form, onClose: () => resolve(confirmed) });

    cancel.onclick = modal.close;
    let saving = false;
    const updateConfirm = () => {
      confirm.disabled = saving || !canConfirm();
    };
    form.oninput = updateConfirm;
    updateConfirm();
    form.onsubmit = async (ev) => {
      ev.preventDefault();
      saving = true;
      updateConfirm();
      const indicator = spinner();
      confirm.prepend(indicator);
      try {
        await onConfirm?.();
        confirmed = true;
        modal.close();
      } catch (err) {
        if (isCancelled(err)) return; // The session closed, so the dialog is already closed too
        error.textContent = err.message;
        error.hidden = false;
        saving = false;
        updateConfirm();
        indicator.remove();
      }
    };
    // First focus: cancel for dangerous work, otherwise the first input (or the confirm button if there is none)
    (danger ? cancel : form.querySelector('input, textarea') ?? confirm).focus();
  });
}

/**
 * Shows a dialog that asks whether to do work that can't be undone.
 *
 * @param {string} message The question (shown as the title)
 * @param {string} confirmLabel Confirm button text
 * @returns {Promise<boolean>} `true` if confirmed
 */
const confirmDialog = (message, confirmLabel) => showDialog({ title: message, confirmLabel, danger: true });

/**
 * Makes a select box for choosing one star list.
 * If a star list is open in the sidebar, it is selected in advance.
 *
 * @param {string} noneLabel Text of the "none" option. Its value is `0`
 * @returns {HTMLSelectElement} Its value is the chosen star list ID (a string)
 */
function starListSelect(noneLabel) {
  const select = el('select');
  fillStarListOptions(select, noneLabel, state.selected);
  return select;
}

/**
 * Refills a select box's options with the current star lists (by name).
 *
 * @param {HTMLSelectElement} select The select box to fill
 * @param {string} noneLabel Text of the "none" option (value `0`)
 * @param {number|string} selectedId Star list ID to select. If no such star list exists, "none" is selected
 */
function fillStarListOptions(select, noneLabel, selectedId) {
  select.replaceChildren(
    el('option', { value: '0', textContent: noneLabel }),
    ...sortedStarLists().map((l) => el('option', { value: String(l.id), textContent: l.name })),
  );
  select.value = starListById(selectedId) ? String(selectedId) : '0';
}

/**
 * Opens the dialog for adding a repository directly.
 * Errors (repository not found, already in the archive and so on) are shown in the dialog, and it stays open.
 *
 * @param {string} [prefill] Value to put in the repository input in advance (from the `add` parameter of the address).
 *   Nothing is saved until the user presses Add
 */
function openAddRepo(prefill = '') {
  const input = el('input', { value: prefill, placeholder: 'owner/repo or GitHub URL', spellcheck: false });
  const inputHint = el('p', { className: 'input-hint invalid', hidden: true }); // Says right away why Add is locked, under the input
  const select = starListSelect('No list');
  // Names shown for the archive items now (`view`: live name, then snapshot, then the stored name). GitHub names ignore case
  const archivedNames = new Set(state.starItems.map((item) => view(item).name.toLowerCase()));
  // State of what is typed now: `empty`, `invalid` (not a repository), `archived` (already in the archive) or `ok`
  const inputState = () => {
    if (!input.value.trim()) return 'empty';
    const fullName = parseRepoInput(input.value);
    if (!fullName) return 'invalid';
    return archivedNames.has(fullName.toLowerCase()) ? 'archived' : 'ok';
  };
  // Colors the input red and sets the message under it when the value can't be added (`inputState`)
  const updateHint = () => {
    const now = inputState();
    const unusable = now === 'invalid' || now === 'archived';
    input.classList.toggle('input-invalid', unusable);
    input.setAttribute('aria-invalid', String(unusable)); // Screen readers announce the same state as the red color
    inputHint.textContent = { invalid: 'Enter owner/repo or a GitHub repository URL.', archived: 'Already in your archive.' }[now] ?? '';
    inputHint.hidden = !inputHint.textContent;
  };
  input.addEventListener('input', updateHint);
  updateHint();
  showDialog({
    title: 'Add repository',
    body: [el('label', {}, 'Repository', input), inputHint, el('label', {}, 'List', select)],
    confirmLabel: 'Add',
    canConfirm: () => inputState() === 'ok',
    onConfirm: async () => {
      const item = await addRepo(input.value, Number(select.value));
      setStatus(`Added ${item.name}`);
    },
  });
}

/**
 * Turns what the user typed (owner/repo or a GitHub address) into `owner/repo`.
 * It follows the rules of the server (`parseRepoFullNameOrURL` in `api/archive-star-items.go`): the address prefix,
 * a query or `#` part, and a path after the repository (like `/tree/main`) are dropped, and a trailing `.git` is removed.
 * It also rejects an owner with `.` in it. GitHub user and organization names can't have one,
 * so such input is an address of another site (like `gitlab.com/owner/repo`) and would only end in a 404 from GitHub.
 *
 * @param {string} value What the user typed
 * @returns {string} `owner/repo`, or an empty string if it doesn't look like a repository
 *
 * NOTE: It only decides what the page shows before saving (for example, "already in your archive").
 * The server parses the value again and makes the real decision, so a difference between the two can't save anything wrong.
 */
function parseRepoInput(value) {
  let s = value.trim().replace(/^https?:\/\//, '').replace(/^www\./, '');
  if (s.toLowerCase().startsWith('github.com/')) s = s.slice('github.com/'.length);
  s = s.split(/[?#]/)[0];
  const [owner = '', name = ''] = s.split('/');
  const repo = name.replace(/\.git$/, '');
  const isPart = (part) => /^[\w.-]+$/.test(part) && part.replace(/^\.+|\.+$/g, '') !== '';
  return isPart(owner) && !owner.includes('.') && isPart(repo) ? `${owner}/${repo}` : '';
}

/**
 * Makes the name and description inputs for a star list.
 *
 * @param {{ name: string, description: string }} [starList] Initial values. Empty if missing
 * @returns {{ nodes: HTMLElement[], values: () => { name: string, description: string } }}
 *   The elements to put in the dialog, and a function that reads the input values with surrounding spaces removed
 */
function starListFields(starList = { name: '', description: '' }) {
  const name = el('input', { value: starList.name, required: true, maxLength: TEXT_LIMITS.starListName, placeholder: 'e.g. Graphics' });
  const description = el('input', { value: starList.description, maxLength: TEXT_LIMITS.starListDescription, placeholder: 'Optional' });
  return {
    nodes: [el('label', {}, 'Name', name), el('label', {}, 'Description', description)],
    values: () => ({ name: name.value.trim(), description: description.value.trim() }),
  };
}

/**
 * Opens the dialog for creating a new star list.
 * After it is created, the sidebar opens it.
 *
 * @returns {Promise<boolean>} `true` if created
 */
function openCreateStarList() {
  const fields = starListFields();
  return showDialog({
    title: 'Create list',
    body: fields.nodes,
    confirmLabel: 'Create',
    onConfirm: async () => {
      const { name, description } = fields.values();
      if (!name) throw new Error('Name is required');
      const starList = await createStarList(name, description);
      setFilter('selected', starList.id);
      setStatus(`Created ${name}`);
    },
  });
}

/**
 * Opens the dialog for editing a star list's name and description.
 *
 * @param {object} starList The star list to edit
 * @returns {Promise<boolean>} `true` if saved
 */
function openEditStarList(starList) {
  const fields = starListFields(starList);
  return showDialog({
    title: 'Edit list',
    body: fields.nodes,
    confirmLabel: 'Save',
    onConfirm: async () => {
      const { name, description } = fields.values();
      if (!name) throw new Error('Name is required');
      await updateStarList(starList.id, name, description);
      setStatus('List saved');
    },
  });
}

/**
 * Asks whether to delete a star list, and deletes it if confirmed.
 * Its repositories stay in the archive.
 *
 * @param {object} starList The star list to delete
 */
async function openDeleteStarList(starList) {
  const ok = await confirmDialog(`Delete the list "${starList.name}"? Its repositories stay in the archive.`, 'Delete');
  if (!ok) return;
  await run(() => deleteStarList(starList.id), 'Deleting list', `Deleted ${starList.name}`);
}

/**
 * Opens the dialog that shows the README snapshot of `item`, as plain text.
 * It fetches the snapshot when it opens (`GET /api/archive-repo-meta?id=`), and shows a loading line until then.
 *
 * @param {object} item Archive item
 *
 * NOTE: The README is someone else's text, so it goes in with `textContent` only, never as HTML.
 * Sync doesn't ask for the README of a repository that is gone, so one gone before its first check never gets one.
 * The message for a README not checked yet says so for those, instead of promising the next sync.
 */
function openReadme(item) {
  const v = view(item);
  const content = el('div', {}, el('p', { className: 'muted' }, spinner(), ' Loading…'));
  const copy = el('button', { type: 'button', className: 'ghost', textContent: 'Copy', hidden: true }); // Shown once a saved README is in
  const close = el('button', { type: 'button', className: 'ghost', textContent: 'Close' });
  const panel = el(
    'div',
    { className: 'dialog-body' },
    el('h2', { textContent: 'Saved README' }),
    el('p', { className: 'muted', textContent: v.name }),
    content,
    el('div', { className: 'actions' }, copy, close),
  );
  const modal = openModal({ panel, className: 'readme-dialog' });
  close.onclick = modal.close;
  (async () => {
    try {
      const { readme } = await api(`archive-repo-meta?id=${item.id}`);
      if (readme.state === 'saved') {
        const text = readme.text ?? '';
        // One element per line, so CSS numbers the lines (`.readme-line`).
        // The numbers aren't text, so selecting and copying leave them out.
        // Each line keeps its line break, so text selected across lines keeps its empty lines (an empty element adds no line break to it)
        const lines = text.replace(/\r?\n$/, '').split(/\r?\n/);
        const pre = el('pre', { className: 'readme-text' }, ...lines.map((line) => el('span', { className: 'readme-line', textContent: `${line}\n` })));
        // The number column is as wide as the largest number.
        // CSP blocks inline `style` attributes, so it is set through CSSOM
        pre.style.setProperty('--line-digits', String(String(lines.length).length));
        content.replaceChildren(pre);
        if (readme.truncated) content.append(el('p', { className: 'readme-truncated', textContent: '(Truncated)' }));
        copy.hidden = false;
        // Timer that puts back the "Copy" label.
        // A new click restarts it, so the label stays 1.5 seconds after the last click
        let copyLabelTimer = null;
        copy.onclick = async () => {
          try {
            await navigator.clipboard.writeText(text);
            copy.textContent = 'Copied';
          } catch {
            copy.textContent = 'Copy failed'; // The browser refused, or has no clipboard here (only HTTPS pages and localhost have one)
          }
          clearTimeout(copyLabelTimer);
          copyLabelTimer = setTimeout(() => (copy.textContent = 'Copy'), 1500);
        };
      } else if (readme.state === 'absent') {
        content.replaceChildren(el('p', { className: 'muted', textContent: 'This repository had no README when it was last checked.' }));
      } else if (readme.state === 'unchecked') {
        const message = v.available ? 'Not saved yet. The next sync saves it.' : 'No README was saved before this repository became unavailable.';
        content.replaceChildren(el('p', { className: 'muted', textContent: message }));
      } else {
        content.replaceChildren(el('p', { className: 'dialog-error', textContent: "The server's answer about this README isn't one this page knows." }));
      }
    } catch (err) {
      if (isCancelled(err)) return; // The session closed, so the dialog is already closed too
      content.replaceChildren(el('p', { className: 'dialog-error', textContent: err.message }));
    }
  })();
}

/**
 * Opens a dialog for choosing an item's star lists with checkboxes.
 * It does the same job as the star list menu next to GitHub's Star button.
 * A star list created inside the dialog is added to the list right away, already checked.
 *
 * @param {object} item Archive item
 * @returns {Promise<boolean>} `true` if saved
 */
function openMembership(item) {
  const current = new Set(starListIdsOf(item));
  const options = el('div', { className: 'star-list-options' });
  const empty = el('p', { className: 'muted', textContent: 'No lists yet. Create one below.', hidden: state.starLists.length > 0 });
  // Adds one checkbox row.
  // Once there is at least one, the "none yet" text is hidden
  const addOption = (starList, checked) => {
    options.append(el('label', { className: 'star-list-option' }, el('input', { type: 'checkbox', value: starList.id, checked }), starList.name));
    empty.hidden = true;
  };
  for (const starList of sortedStarLists()) addOption(starList, current.has(starList.id));

  const newName = el('input', { placeholder: 'New list name', maxLength: TEXT_LIMITS.starListName });
  const createButton = el('button', { type: 'button', className: 'ghost', textContent: 'Create' });
  const createError = el('p', { className: 'dialog-error', hidden: true }); // Error of Create. It shows under the create row, because the page status line is behind the dialog's backdrop
  createButton.onclick = async () => {
    const name = newName.value.trim();
    if (!name) return newName.focus();
    createError.hidden = true;
    createButton.disabled = true;
    const indicator = spinner();
    createButton.prepend(indicator);
    try {
      addOption(await createStarList(name, ''), true);
      newName.value = '';
    } catch (err) {
      if (isCancelled(err)) return; // The session closed, so the dialog is already closed too
      createError.textContent = err.message;
      createError.hidden = false;
    }
    indicator.remove();
    createButton.disabled = false;
  };
  // Enter in the name input creates a star list.
  // It doesn't save the dialog (submit the form)
  newName.onkeydown = (ev) => {
    if (ev.key === 'Enter') {
      ev.preventDefault();
      createButton.click();
    }
  };

  return showDialog({
    title: `Lists for ${view(item).name}`,
    body: [
      empty,
      options,
      el('div', { className: 'new-star-list' }, newName, createButton),
      createError,
    ],
    confirmLabel: 'Save',
    onConfirm: async () => {
      const checked = [...options.querySelectorAll('input:checked')].map((i) => Number(i.value)); // Checkbox values are strings
      await updateStarItem(item.id, { star_list_ids: checked });
      setStatus('Lists saved');
    },
  });
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

/**
 * Returns a new array of star lists sorted by name (ignoring case).
 *
 * @returns {object[]}
 */
const sortedStarLists = () => [...state.starLists].sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }));

// Archive item -> `{ text, settled }` (the search text, and whether it was built after the live details were settled).
// A changed item is replaced by the new object from the server, so old values are dropped on their own (`WeakMap`)
const searchTextCache = new WeakMap();

/**
 * Returns the item's search text (name, description, note, language and topics joined in lowercase).
 *
 * @param {object} item Archive item
 * @returns {string}
 *
 * NOTE: Live details are fetched only for items shown on screen.
 * So most items use text built from the snapshot (or the saved name if there is none).
 * That text is cached too, and rebuilt when live details arrive later (when the settled state changes).
 * Without the cache, every keystroke would recompute every item.
 */
function searchText(item) {
  const settled = state.liveMeta.has(item.node_id) || state.missing.has(item.node_id);
  const cached = searchTextCache.get(item);
  if (cached?.settled === settled) return cached.text;
  const v = view(item);
  const text = [v.name, v.description, item.note, v.language, ...v.topics].join('\n').toLowerCase();
  searchTextCache.set(item, { text, settled });
  return text;
}

/**
 * Checks whether the item belongs to the sidebar entry chosen (`state.selected`).
 *
 * @param {object} item Archive item
 * @returns {boolean}
 */
function inSelectedList(item) {
  if (state.selected === ALL) return true;
  if (state.selected === UNLISTED) return starListIdsOf(item).length === 0;
  if (state.selected === UNAVAILABLE) return !view(item).available;
  return starListIdsOf(item).includes(state.selected);
}

/**
 * Builds the list with the sidebar choice, language filter, search text and sort order applied.
 * The search text is split on spaces, and only items that contain every word remain.
 *
 * @returns {object[]} Archive items. `newest` and `oldest` sort by `starred_at` (when it was starred, or when it was added for items added directly)
 *
 * NOTE: `starred_at` values mix formats with and without milliseconds (`2026-01-01T00:00:00Z` and `2026-01-01T00:00:00.000Z`).
 * So they are compared as times, not as strings.
 */
function filteredStarItems() {
  const terms = state.query.toLowerCase().split(/\s+/).filter(Boolean);
  const items = state.starItems.filter(
    (item) =>
      inSelectedList(item) &&
      (!state.language || view(item).language === state.language) &&
      terms.every((t) => searchText(item).includes(t)),
  );
  if (state.sort === 'name') return items.sort((a, b) => view(a).name.localeCompare(view(b).name));
  const starredTime = new Map(items.map((item) => [item, Date.parse(item.starred_at) || 0])); // Parsed in advance, so sorting doesn't parse the same value again and again
  const sign = state.sort === 'oldest' ? 1 : -1;
  return items.sort((a, b) => sign * (starredTime.get(a) - starredTime.get(b)));
}

/**
 * Refills the language select box from the current data.
 * The most used languages come first.
 * If the chosen language no longer exists (for example, live details changed the language), it goes back to "all languages".
 */
function renderLanguageOptions() {
  const counts = new Map();
  for (const item of state.starItems) {
    const lang = view(item).language;
    counts.set(lang, (counts.get(lang) ?? 0) + 1);
  }
  const select = $('language');
  const options = [...counts]
    .filter(([lang]) => lang)
    .sort((a, b) => b[1] - a[1])
    .map(([lang, n]) => el('option', { value: lang, textContent: `${lang} (${n})` }));
  select.replaceChildren(el('option', { value: '', textContent: 'All languages' }), ...options);
  select.value = counts.has(state.language) ? state.language : '';
  state.language = select.value;
}

/**
 * Draws the left sidebar: all, unlisted, unavailable repositories (only if there are any),
 * each star list with its item count, and the new star list button.
 */
function renderSidebar() {
  const counts = new Map();
  let unlisted = 0;
  let unavailable = 0;
  for (const item of state.starItems) {
    const ids = starListIdsOf(item);
    if (!ids.length) unlisted++;
    if (!view(item).available) unavailable++;
    for (const id of ids) counts.set(id, (counts.get(id) ?? 0) + 1);
  }

  // One sidebar entry button.
  // It has a focus key, so keyboard focus survives a redraw
  const link = (value, label, count) => {
    const button = el(
      'button',
      { type: 'button', className: `nav-item${state.selected === value ? ' active' : ''}`, onclick: () => setFilter('selected', value) },
      el('span', { className: 'nav-label', textContent: label }),
      el('span', { className: 'count', textContent: String(count) }),
    );
    if (state.selected === value) button.setAttribute('aria-current', 'true');
    button.dataset.focusKey = `nav:${value}`;
    return button;
  };

  // `replaceChildren` inserts `false` as text, so entries left out by a condition are filtered out
  const parts = [
    link(ALL, 'All stars', state.starItems.length),
    link(UNLISTED, 'Unlisted', unlisted),
    (unavailable > 0 || state.selected === UNAVAILABLE) && link(UNAVAILABLE, 'Unavailable', unavailable),
    el('h2', { className: 'nav-heading', textContent: `Lists (${state.starLists.length})` }),
    ...sortedStarLists().map((l) => link(l.id, l.name, counts.get(l.id) ?? 0)),
    el('button', { type: 'button', className: 'nav-item new', onclick: openCreateStarList }, icon('plus'), 'New list'),
  ];
  $('sidebar').replaceChildren(...parts.filter(Boolean));
}

/**
 * Draws the title area above the list.
 * When a star list is open, it shows its name, description, and edit and delete icons.
 */
function renderListHeader() {
  const starList = starListById(state.selected);
  const title = starList ? starList.name : { [UNLISTED]: 'Unlisted', [UNAVAILABLE]: 'Unavailable' }[state.selected] ?? 'All stars';
  // `replaceChildren` inserts `undefined` as text, so a missing description is filtered out
  const parts = [
    el(
      'div',
      { className: 'list-title' },
      el('h2', { textContent: title }),
      starList &&
        el(
          'div',
          { className: 'item-actions' },
          iconButton('pencil', 'Edit list', () => openEditStarList(starList)),
          iconButton('trash', 'Delete list', () => openDeleteStarList(starList), 'danger'),
        ),
    ),
    starList?.description && el('p', { className: 'desc', textContent: starList.description }),
  ];
  $('list-header').replaceChildren(...parts.filter(Boolean));
}

const DATE = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', year: 'numeric' }); // Date format: Oct 14, 2026
const DATE_TIME = new Intl.DateTimeFormat('en-US', { dateStyle: 'medium', timeStyle: 'short' }); // Date and time format: Oct 14, 2026, 6:43 AM. In the browser's time zone, like `DATE`
const COUNT = new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 }); // Short form of large numbers: 1234 -> 1.2K

/**
 * Makes a date element.
 * On hover, it also shows the time.
 *
 * @param {string} prefix Word before the date (e.g. "Starred")
 * @param {string} iso ISO 8601 time
 * @returns {HTMLTimeElement}
 */
function dateLabel(prefix, iso) {
  const date = new Date(iso);
  return el('time', { dateTime: iso, title: DATE_TIME.format(date), textContent: `${prefix} ${DATE.format(date)}` });
}

/**
 * Opens or closes an item's note editor.
 * If closed, it makes a draft from the current note and opens it.
 * If open, it drops the draft and closes it.
 * Editors of other items are not affected.
 * It doesn't close while saving.
 *
 * @param {object} item Archive item
 */
function toggleNoteEditor(item) {
  const note = state.noteDrafts.get(item.id);
  if (!note) state.noteDrafts.set(item.id, { draft: item.note ?? '', saving: false });
  else if (!note.saving) state.noteDrafts.delete(item.id);
  render();
  focusByKey(`note:${item.id}`);
}

/**
 * Saves an item's note draft.
 * On success, it closes that editor.
 * On failure, it keeps the text so the user can fix it and try again.
 *
 * @param {number} id Archive item ID
 */
async function saveNote(id) {
  const note = state.noteDrafts.get(id);
  if (!note || note.saving) return;
  note.saving = true;
  render();
  const ok = await run(() => updateStarItem(id, { note: note.draft }), 'Saving note', 'Note saved');
  if (state.noteDrafts.get(id) !== note) return; // Signed out (`clearLoadedData` replaced the map) or the item was removed in the meantime
  if (ok) state.noteDrafts.delete(id);
  else note.saving = false;
  render();
  // On failure, go back to the input so the user can fix it.
  // Don't take focus from another editor the user is typing in
  if (!ok && document.activeElement === document.body) focusByKey(`note:${id}`);
}

/**
 * Makes the note editor (input, save, cancel).
 * Typing goes straight into the draft (`note.draft`), so a redraw keeps the text.
 *
 * @param {object} item Archive item
 * @param {{ draft: string, saving: boolean }} note This item's draft (a value of `state.noteDrafts`). While saving, the editor is locked and shows progress
 * @returns {HTMLFormElement}
 */
function noteEditor(item, note) {
  const input = el('textarea', { value: note.draft, rows: 3, maxLength: TEXT_LIMITS.note, placeholder: 'Add a note', disabled: note.saving });
  withFocusKey(input, `note:${item.id}`);
  input.oninput = () => {
    note.draft = input.value;
  };
  const cancel = el('button', { type: 'button', className: 'ghost', textContent: 'Cancel', disabled: note.saving, onclick: () => toggleNoteEditor(item) });
  const form = el('form', { className: 'note-edit' }, input, el('div', { className: 'actions' }, el('button', { type: 'submit', disabled: note.saving }, note.saving && spinner(), 'Save'), cancel));
  form.onsubmit = (ev) => {
    ev.preventDefault();
    saveNote(item.id);
  };
  return form;
}

/**
 * Draws one archive item, laid out like GitHub's Stars tab.
 * `view` decides the repository details.
 *
 * @param {object} item Archive item
 * @returns {HTMLElement}
 */
function renderItem(item) {
  const v = view(item);
  const [owner, repo] = v.name.split('/');
  const url = safeUrl(v.url);
  const title = el('span', {}, `${owner} / `, el('strong', { textContent: repo }));

  const note = state.noteDrafts.get(item.id);
  const noteBox = el('div', { className: 'note-box' }, note ? noteEditor(item, note) : item.note && el('p', { className: 'note', textContent: item.note }));

  const remove = async () => {
    if (!(await confirmDialog(`Remove ${v.name} from the archive?`, 'Remove'))) return;
    await run(() => removeStarItem(item.id), 'Removing', `Removed ${v.name}`);
  };

  const language =
    v.language &&
    el(
      'button',
      { type: 'button', className: 'meta-link', title: 'Show only this language', onclick: () => setFilter('language', v.language) },
      el('span', { className: 'lang-dot' }),
      v.language,
    );
  // CSP blocks inline `style` attributes but not CSSOM, so the color is set with `style.setProperty`
  if (language && v.languageColor) language.style.setProperty('--lang-color', v.languageColor);

  // Star and fork counts.
  // Not drawn if the value is `null` because there are no live details
  const stat = (iconName, count, label) =>
    count !== null && el('span', { className: 'stat', title: `${count.toLocaleString('en-US')} ${label}` }, icon(iconName), COUNT.format(count));

  const memberOf = starListIdsOf(item).map((id) => starListById(id));

  return el(
    'article',
    { className: `item${v.available ? '' : ' unavailable'}` },
    el(
      'div',
      { className: 'item-head' },
      el('h3', {}, url ? el('a', { href: url, target: '_blank', rel: 'noopener noreferrer' }, title) : title),
      el(
        'div',
        { className: 'item-actions' },
        withFocusKey(iconButton('book', 'Saved README', () => openReadme(item)), `item:${item.id}:readme`),
        withFocusKey(iconButton('list', 'Lists', () => openMembership(item)), `item:${item.id}:lists`),
        withFocusKey(iconButton('pencil', item.note ? 'Edit note' : 'Add note', () => toggleNoteEditor(item)), `item:${item.id}:note`),
        withFocusKey(iconButton('trash', 'Remove from archive', remove, 'danger'), `item:${item.id}:remove`),
      ),
    ),
    v.description && el('p', { className: 'desc', textContent: v.description }),
    v.topics.length > 0 &&
      el(
        'div',
        { className: 'topics' },
        ...v.topics.map((topic) =>
          el('button', { type: 'button', className: 'topic', textContent: topic, title: 'Search this topic', onclick: () => setFilter('query', topic) }),
        ),
      ),
    noteBox,
    el(
      'div',
      { className: 'meta' },
      !v.available &&
        el('span', {
          className: 'badge warn',
          textContent: v.unavailableSince ? `Unavailable since ${DATE.format(new Date(v.unavailableSince))}` : 'Unavailable',
          title: 'GitHub can no longer find this repository (deleted, made private, or blocked). The details shown are the last snapshot.',
        }),
      v.archived && el('span', { className: 'badge warn', textContent: 'Archived', title: 'Archived by its owner (read-only)' }),
      language,
      stat('star', v.stars, 'stars'),
      stat('fork', v.forks, 'forks'),
      v.pushedAt && dateLabel('Updated', v.pushedAt),
      ...memberOf.map((starList) =>
        el('button', { type: 'button', className: 'meta-link star-list-chip', textContent: starList.name, title: 'Open this list', onclick: () => setFilter('selected', starList.id) }),
      ),
      item.starred_at && dateLabel('Starred', item.starred_at),
      v.previousName && el('span', { className: 'muted-inline', textContent: `Renamed from ${v.previousName}` }),
    ),
  );
}

/**
 * Puts a focus key on an element.
 * An element with this key keeps its focus and cursor position when `render` rebuilds it (`captureFocus`, `restoreFocus`).
 *
 * @param {HTMLElement} node The element to put the key on
 * @param {string} key A key unique on the whole page (e.g. `note:3`)
 * @returns {HTMLElement} The same element (so it can be used inline while building elements)
 */
function withFocusKey(node, key) {
  node.dataset.focusKey = key;
  return node;
}

/**
 * Focuses the element with the given focus key.
 * Does nothing if there is no such element.
 *
 * @param {string} key Focus key
 */
function focusByKey(key) {
  document.querySelector(`[data-focus-key="${CSS.escape(key)}"]`)?.focus();
}

/**
 * Remembers the focus key, cursor position and scroll position of the focused element.
 *
 * @returns {{ key: string, start: number|null, end: number|null, scrollTop: number }|null} `null` if the element has no focus key
 */
function captureFocus() {
  const node = document.activeElement;
  const key = node?.dataset?.focusKey;
  if (!key) return null;
  return { key, start: node.selectionStart ?? null, end: node.selectionEnd ?? null, scrollTop: node.scrollTop };
}

/**
 * Restores the focus, cursor and scroll position saved by `captureFocus` to the new element with the same key.
 *
 * @param {{ key: string, start: number|null, end: number|null, scrollTop: number }|null} saved The saved values. Does nothing if `null`
 */
function restoreFocus(saved) {
  const node = saved && document.querySelector(`[data-focus-key="${CSS.escape(saved.key)}"]`);
  if (!node || node === document.activeElement) return;
  node.focus({ preventScroll: true });
  if (saved.start !== null) {
    try {
      node.setSelectionRange(saved.start, saved.end);
    } catch {} // Skip input types that have no cursor
  }
  node.scrollTop = saved.scrollTop;
}

/**
 * Redraws the whole page.
 * It builds DOM only for the first `state.shown` filtered items, and requests live details for them.
 *
 * NOTE: The page is drawn only from `state`, so calling this is always safe.
 * Things a redraw would lose (text being typed, focus, cursor) are kept in `state.noteDrafts` and focus keys.
 * So there are no exceptions like "don't draw while editing".
 */
function render() {
  const focus = captureFocus(); // After the redraw, focus and cursor go back to the new element with the same focus key (`restoreFocus`)
  // If the open star list was deleted (for example, on another device), go back to all
  if (![ALL, UNLISTED, UNAVAILABLE].includes(state.selected) && !starListById(state.selected)) state.selected = ALL;

  renderSidebar();
  renderListHeader();
  renderLanguageOptions();
  const filtered = filteredStarItems();
  const visible = filtered.slice(0, state.shown);

  $('list').replaceChildren(
    ...(visible.length ? visible.map(renderItem) : [el('p', { className: 'empty', textContent: state.starItems.length ? 'No matches' : 'No repositories yet' })]),
  );
  const total = state.starItems.filter(inSelectedList).length;
  $('summary').textContent = filtered.length === total ? `${total} repositories` : `${filtered.length} of ${total}`;

  const more = $('more');
  more.hidden = filtered.length <= state.shown;
  more.textContent = `Load more (${visible.length} / ${filtered.length})`;
  restoreFocus(focus);
  requestLiveMeta(visible); // Fetch in the background only for drawn items not asked about yet
}

/**
 * Changes one filter, shows it in its input box, and redraws from the first page at the top.
 *
 * @param {'selected'|'query'|'language'|'sort'} key The `state` key to change
 * @param {string|number} value New value
 */
function setFilter(key, value) {
  state[key] = value;
  const input = { query: 'search', language: 'language', sort: 'sort' }[key];
  if (input) $(input).value = value;
  state.shown = PAGE_SIZE;
  render();
  window.scrollTo({ top: 0 });
}

// ---------------------------------------------------------------------------
// Importing GitHub Stars (Import)
// ---------------------------------------------------------------------------

const STARRED_PARALLEL = 6; // Pages of GitHub Stars fetched in parallel

/**
 * Fetches all of my GitHub Stars.
 * The first page tells the last page number, and then the rest are fetched in parallel.
 *
 * @param {(done: number, total: number) => void} onProgress Called each time a page arrives (pages received, total pages)
 * @returns {Promise<{ starredRepos: object[], githubStarLists: Object<string, string[]>, githubStarListsFailed: boolean }>}
 *   The Stars (each repository once), repository ID -> names of the GitHub star lists it is in (for display),
 *   and whether the GitHub star lists couldn't be read
 *
 * NOTE: If the list changes while pages are fetched, the same repository can come on two pages.
 * So they are merged by ID.
 */
async function fetchAllStarred(onProgress) {
  const first = await api('github-stars?page=1');
  const pages = [first.starred_repos];
  let nextPage = 2;
  let done = 1;
  onProgress(done, first.last_page);
  const worker = async () => {
    while (nextPage <= first.last_page) {
      const page = nextPage++;
      pages[page - 1] = (await api(`github-stars?page=${page}`)).starred_repos;
      onProgress(++done, first.last_page);
    }
  };
  await Promise.all(Array.from({ length: STARRED_PARALLEL }, worker));

  // Keep only one copy of a repository that came on two pages because the list changed while fetching
  const byId = new Map();
  for (const s of pages.flat()) if (!byId.has(s.repo.id)) byId.set(s.repo.id, s);
  // GitHub star lists are only for display.
  // If the server can't read them, only `github_star_lists_failed` comes.
  // The Stars are still shown
  return { starredRepos: [...byId.values()], githubStarLists: first.github_star_lists ?? {}, githubStarListsFailed: first.github_star_lists_failed === true };
}

/**
 * Makes one row for a new Star that can be chosen in the Import dialog.
 * The row has a checkbox, the name, GitHub star lists, the Star count and the date it was starred.
 *
 * @param {object} starredRepo One GitHub Star (an entry of `/api/github-stars`)
 * @param {boolean} checked Initial checked state
 * @param {string[]} githubStarListNames Names of the GitHub star lists this repository is in
 * @returns {{ row: HTMLElement, input: HTMLInputElement, starredRepo: object }} The row element, its checkbox and the given Star
 */
function importRow(starredRepo, checked, githubStarListNames) {
  const input = el('input', { type: 'checkbox', checked });
  const row = el(
    'label',
    { className: 'import-row' },
    input,
    el('span', { className: 'import-name', textContent: starredRepo.repo.full_name }),
    ...githubStarListNames.map((name) => el('span', { className: 'star-list-chip', textContent: name, title: 'GitHub list. Kept when imported' })),
    el('span', { className: 'stat' }, icon('star'), COUNT.format(starredRepo.repo.stargazers_count)),
    el('span', { className: 'muted', textContent: DATE.format(new Date(starredRepo.starred_at)) }),
  );
  return { row, input, starredRepo };
}

// IDs of repositories whose page was opened through the GitHub link.
// It is remembered when the dialog opens again, until the page is reloaded
const openedOnGitHub = new Set();

/**
 * Makes one row for a Star already in the archive.
 * It looks disabled, so it can't be chosen.
 * It has a link to the repository page, so the user can unstar it on GitHub directly.
 * A row whose link was pressed is marked "Opened", to show how far the user got.
 *
 * @param {object} starredRepo One GitHub Star (an entry of `/api/github-stars`)
 * @returns {HTMLElement}
 */
function archivedImportRow(starredRepo) {
  const url = safeUrl(`https://github.com/${starredRepo.repo.full_name}`);
  const checkbox = el('input', { type: 'checkbox', checked: true, disabled: true });
  checkbox.setAttribute('aria-label', 'Already in the archive');
  const badge = el('span', { className: 'badge warn' });
  const row = el('div', { className: 'import-row archived' }, checkbox, el('span', { className: 'import-name', textContent: starredRepo.repo.full_name }), badge);
  // Sets the row's look based on whether it was opened before
  const markOpened = () => {
    const opened = openedOnGitHub.has(starredRepo.repo.id);
    row.classList.toggle('opened', opened);
    badge.textContent = opened ? 'Opened' : 'In archive';
  };
  if (url) {
    // Keep the link's default action (open a new tab), and only change the look
    const link = el('a', { href: url, target: '_blank', rel: 'noopener noreferrer', className: 'icon-link', title: 'Open on GitHub' }, icon('linkExternal'));
    link.setAttribute('aria-label', `Open ${starredRepo.repo.full_name} on GitHub`);
    link.onclick = () => {
      openedOnGitHub.add(starredRepo.repo.id);
      markOpened();
    };
    row.append(link);
  }
  markOpened();
  return row;
}

/**
 * Opens the dialog for choosing GitHub Stars and importing them into the archive.
 *   1. Fetches all GitHub Stars and splits them into "new Stars" (not in the archive) and "Stars already archived"
 *   2. Sends only the IDs of the chosen new Stars to the server in one request.
 *      The server reads repository details and star lists from GitHub itself, and checks for duplicates at commit time
 *   3. Stars already archived are shown too, disabled.
 *      To remove one from the public list of Stars, the user unstars it on GitHub through its link.
 *      After "Refresh" loads the list again, unstarred ones drop out
 *
 * NOTE: The result of the add is not guessed.
 * The archive is fetched again to check it.
 * The commit may have happened even if the request failed (the server committed, but the response was lost).
 * Sending an Import again gives the same result, so if the result can't be checked, the user is asked to press it again.
 */
async function openImport() {
  let closed = false; // Whether the dialog closed (including when the session closed). After it closes, the page is not changed
  let busy = false; // `true` while loading the list or adding. All controls are locked then (`updateControls`)
  const status = el('p', { className: 'muted' });
  // Shows work in progress in the status line, with the progress indicator in front.
  // Setting `textContent` later removes the indicator
  const showProgress = (text) => {
    status.className = 'muted';
    status.replaceChildren(spinner(), text);
  };
  const content = el('div', { className: 'import-content' });
  const cancel = el('button', { type: 'button', className: 'ghost', textContent: 'Cancel' });
  const refresh = el('button', { type: 'button', className: 'ghost', textContent: 'Refresh', disabled: true, title: 'Load your GitHub stars again' });
  const confirm = el('button', { type: 'button', textContent: 'Import', disabled: true });
  const panel = el('div', { className: 'dialog-body' }, el('h2', { textContent: 'Import GitHub stars' }), status, content, el('div', { className: 'actions' }, refresh, cancel, confirm));
  const modal = openModal({
    panel,
    className: 'import-dialog',
    onClose: () => {
      closed = true;
    },
  });
  cancel.onclick = modal.close;

  const starListPicker = starListSelect('No extra list');
  let githubStarLists = {}; // Repository ID -> GitHub star list names (for display. When adding, the server reads them from GitHub again)
  let githubStarListsFailed = false; // `true` if the server couldn't read the GitHub star lists
  let starredRepos = []; // The GitHub Stars received last
  let loadedOnce = false; // Whether the Stars were ever received. Decides whether Refresh adds a "compared to last time" message
  let rows = []; // Rows of new Stars shown now
  let selectAll = null; // Select-all checkbox for new Stars (`null` if there are no new Stars)

  // New Stars checked now
  const selected = () => rows.filter((r) => r.input.checked).map((r) => r.starredRepo);
  // Sets the controls again from the current values (`busy`, each row's checkbox).
  // Locking, the Import button text and the select-all checkbox are all set only here.
  // So however work ends and however checks change, calling this makes the page match the real state
  const updateControls = () => {
    const n = selected().length;
    confirm.textContent = n ? `Import ${n}` : 'Import';
    confirm.disabled = busy || !n;
    refresh.disabled = busy;
    starListPicker.disabled = busy;
    if (selectAll) {
      selectAll.disabled = busy;
      selectAll.checked = n > 0 && n === rows.length;
      selectAll.indeterminate = n > 0 && n < rows.length; // Mixed mark when only some are chosen
    }
    for (const r of rows) r.input.disabled = busy;
  };

  // Redraws the list based on the current archive (`state.starItems`).
  // `isChecked(starredRepo)` sets the initial checked state of each new Star row
  const renderContent = (isChecked) => {
    const archivedIds = new Set(state.starItems.map((item) => item.id));
    const fresh = starredRepos.filter((s) => !archivedIds.has(s.repo.id));
    const inArchive = starredRepos.filter((s) => archivedIds.has(s.repo.id));
    status.className = 'muted';
    status.textContent = `${starredRepos.length} ${starredRepos.length === 1 ? 'star' : 'stars'} on GitHub`;

    // Star lists may have been created by Import or deleted elsewhere, so the options are refilled on every draw (the choice is kept)
    fillStarListOptions(starListPicker, 'No extra list', Number(starListPicker.value));
    rows = fresh.map((s) => importRow(s, isChecked(s), githubStarLists[s.repo.id] ?? []));
    for (const r of rows) r.input.onchange = updateControls;
    selectAll = fresh.length > 0 ? el('input', { type: 'checkbox' }) : null; // `updateControls` sets its checked state
    if (selectAll) {
      selectAll.onchange = () => {
        for (const r of rows) r.input.checked = selectAll.checked;
        updateControls();
      };
    }

    // `replaceChildren` inserts `false` as text, so entries left out by a condition are filtered out
    const parts = [
      starredRepos.length
        ? el(
            'div',
            { className: 'import-list' },
            fresh.length > 0 && el('label', { className: 'import-row import-head' }, selectAll, `Not archived yet (${fresh.length})`),
            ...rows.map((r) => r.row),
            inArchive.length > 0 &&
              el(
                'div',
                { className: 'import-row import-head' },
                `Already in archive (${inArchive.length})`,
                // Everything in this list is still starred on GitHub
                el('span', { className: 'muted-inline', textContent: 'Unstar on GitHub to keep them private.' }),
              ),
            ...inArchive.map(archivedImportRow),
          )
        : el('p', { className: 'muted', textContent: 'You have no stars on GitHub.' }),
      fresh.length > 0 && el('label', { className: 'check-option' }, 'Also add to', starListPicker),
      fresh.length > 0 &&
        Object.keys(githubStarLists).length > 0 &&
        el('p', { className: 'muted', textContent: 'GitHub lists are kept on import.' }),
      fresh.length > 0 && githubStarListsFailed && el('p', { className: 'note', textContent: "GitHub lists couldn't be loaded." }),
    ];
    content.replaceChildren(...parts.filter(Boolean));
    updateControls();
  };

  // Fetches the GitHub Stars (again).
  // On failure, it shows the error in the status line and returns `false`
  const loadStars = async () => {
    busy = true;
    updateControls();
    showProgress('Loading your GitHub stars…');
    try {
      ({ starredRepos, githubStarLists, githubStarListsFailed } = await fetchAllStarred((done, total) => showProgress(`Loading your GitHub stars… page ${done} / ${total}`)));
      loadedOnce = true;
      return true;
    } catch (err) {
      if (isCancelled(err)) return false;
      status.className = 'dialog-error';
      status.textContent = err.message;
      return false;
    } finally {
      busy = false;
      updateControls();
    }
  };

  // Fetches the list again, and tells how many Stars were unstarred on GitHub since the last load
  refresh.onclick = async () => {
    const hadList = loadedOnce;
    const before = new Set(starredRepos.map((s) => s.repo.id));
    const checkedIds = new Set(selected().map((s) => s.repo.id));
    if (!(await loadStars()) || closed) return;
    // New Stars already shown keep their checked state.
    // Stars added in the meantime are shown unchecked
    renderContent((s) => checkedIds.has(s.repo.id));
    if (!hadList) return; // There is no earlier list to compare with
    const now = new Set(starredRepos.map((s) => s.repo.id));
    const gone = [...before].filter((id) => !now.has(id)).length;
    for (const id of before) if (!now.has(id)) openedOnGitHub.delete(id); // Ones unstarred and gone from the list also lose their "opened" record
    status.textContent = `${gone} unstarred since the last load. ${status.textContent}`;
  };

  // Adds the chosen Stars, and checks the result by fetching the archive again
  confirm.onclick = async () => {
    const toImport = selected();
    const ids = toImport.map((s) => s.repo.id);
    busy = true;
    updateControls();
    showProgress(`Importing ${COUNT.format(toImport.length)}…`);

    let importError = null;
    let added = 0;
    let skipped = 0; // Chosen ones the server didn't add (already archived, or no longer starred)
    try {
      // Send only the IDs of the chosen repositories in one request.
      // The server reads repository details and star lists from GitHub itself, and skips ones already there
      ({ added, skipped } = await api('archive-star-items', { method: 'POST', body: { starred_repo_ids: ids, star_list_id: Number(starListPicker.value) } }));
    } catch (err) {
      if (isCancelled(err)) return; // The session closed, so the dialog is already closed too. Don't send the next requests
      importError = err;
    }
    let reloaded = true;
    try {
      [state.starItems, state.starLists] = await Promise.all([api('archive-star-items'), api('archive-star-lists')]); // Importing GitHub star lists may have created new star lists
      render(); // `render` requests live details for the new items shown on screen
    } catch (err) {
      if (isCancelled(err)) return;
      reloaded = false;
    }
    busy = false;
    if (closed) return; // Closed (a closed session also ends here)
    cancel.textContent = 'Close';
    const stars = (n) => `${n} ${n === 1 ? 'star' : 'stars'}`;
    const nothingImported = 'Nothing imported. The chosen stars were already archived or are no longer starred.';

    if (!reloaded) {
      // If the archive couldn't be fetched, keep the list as is (with the same choices), so the user can press again
      updateControls();
      status.className = importError ? 'dialog-error' : 'muted';
      if (importError) {
        status.textContent = `${importError.message}. Couldn't confirm the import. Press Import again.`;
      } else if (added === 0) {
        status.textContent = nothingImported;
      } else {
        status.textContent = `Imported ${stars(added)}. Reload to see them.`;
        setStatus(`Imported ${stars(added)}.`);
      }
      return;
    }
    // On success, the remaining new Stars were left out on purpose, so they stay unchecked.
    // On failure, the ones not added yet stay chosen
    const chosen = new Set(ids);
    renderContent((s) => importError !== null && chosen.has(s.repo.id));
    if (importError) {
      const archivedIds = new Set(state.starItems.map((item) => item.id));
      status.className = 'dialog-error';
      status.textContent = `${importError.message}. ${ids.filter((id) => archivedIds.has(id)).length} of ${ids.length} are in the archive.`;
      return;
    }
    if (added === 0) {
      status.textContent = nothingImported;
      return;
    }
    const details = [
      skipped > 0 && `${skipped} skipped (already archived or no longer starred)`,
      rows.length > 0 && `${rows.length} not archived yet`,
    ].filter(Boolean);
    status.replaceChildren(el('span', { className: 'status-success', textContent: `✓ Imported ${stars(added)}` }), ...details.map((text) => ` · ${text}`));
    setStatus(`Imported ${stars(added)}.`);
  };

  // Fetch the list for the first time after all handlers are attached.
  // If it fails, Refresh can fetch it again
  if ((await loadStars()) && !closed) renderContent(() => true);
}

// ---------------------------------------------------------------------------
// Settings (sync)
// ---------------------------------------------------------------------------

// Auto sync intervals (days) to choose from, with their display names.
// 0 is Manual only (only a run started by hand, such as Sync now, syncs).
// They must match the values the server accepts (`core.syncIntervalChoices`)
const SYNC_INTERVALS = [
  [1, 'Every day'],
  [2, 'Every 2 days'],
  [3, 'Every 3 days'],
  [7, 'Every week'],
  [0, 'Manual only'],
];

const DAY_MS = 24 * 60 * 60 * 1000; // One day (milliseconds)

// With no scheduled run for this long, sync is treated as stopped.
// The workflow runs every day whatever the interval, even with Manual only.
// But GitHub can delay or skip scheduled runs when busy (docs, measured).
// So this is three days, to avoid warning about one skipped day
const SYNC_STALL_MS = 3 * DAY_MS;

/**
 * Checks whether sync has a problem the user should know about.
 *
 * @param {object|null} status Sync status from the server (the `state.sync` shape, `core.SyncStatus`):
 *   `enabled_since` (when the user turned it on, `null` if off), `workflow` (GitHub's workflow state, or `missing`),
 *   `last_scheduled_run` (`run_started_at`, `conclusion`, `html_url`)
 * @param {string} readError Why the status couldn't be read (the `state.syncError` shape). An empty string if it was read
 * @returns {{ text: string, hint: string, fix: 'on'|'off'|null }|null} The message, the fix to add in Settings,
 *   and whether the fix button in Settings turns the workflow on (`on`) or off (`off`).
 *   `fix` is `null` if a button can't fix it. `null` if there is no problem
 *
 * NOTE: A sync that stops silently is the worst failure for this tool, so it gets its own warning.
 * The state the user set (Settings) can differ from GitHub's real state
 * (changed in the Actions tab, or turned off by GitHub for inactivity).
 * Then it only warns, and the user fixes it with a button.
 * It never turns anything on or off by itself.
 * While on, it warns when the last scheduled run failed,
 * and when there has been no run for more than three days (Actions blocked, so no failed run and no GitHub notice).
 * Both look only at what happened after the user turned it on (`enabled_since`).
 * This avoids warnings right after turning it on again after a long time,
 * caused by the gap while it was off or by earlier failures.
 */
function syncProblem(status, readError) {
  if (readError) {
    // Don't treat "couldn't read" as "no problem".
    // Don't guess the cause, and show the error message as is
    return {
      text: "Couldn't read the sync status",
      hint: `If this keeps happening, check the access token's Actions permission. Details: ${readError}`,
      fix: null,
    };
  }
  if (!status) return null;
  if (status.workflow === 'missing') {
    // GitHub can answer the same 404 when the workflow file is missing and when the token has no Actions permission.
    // So the cause is not guessed.
    // The fix for both is the setup tool
    return { text: "Couldn't find the sync workflow in your archive repository", hint: 'Run the starkive setup tool again.', fix: null };
  }
  const running = status.workflow === 'active';
  if (!status.enabled_since) {
    // The user turned it off.
    // If the workflow is on, its scheduled runs still call the dashboard every day, against the setting
    if (!running) return null;
    return { text: 'Sync is off in Settings, but its workflow is on in GitHub Actions', hint: 'Turn the workflow off, or turn sync on.', fix: 'off' };
  }
  if (status.workflow === 'disabled_inactivity') {
    return { text: 'GitHub turned off the sync workflow after 60 days without activity in your archive repository', hint: 'Turn it on again here.', fix: 'on' };
  }
  if (!running) {
    return { text: 'Sync is on in Settings, but its workflow is off in GitHub Actions', hint: 'Turn it on again, or turn sync off.', fix: 'on' };
  }
  // Runs before the time it was turned on belong to before it was off, so they are ignored
  const enabledAt = new Date(status.enabled_since).getTime();
  const lastRun = status.last_scheduled_run;
  const last = lastRun && new Date(lastRun.run_started_at).getTime() >= enabledAt ? lastRun : null;
  if (last && last.conclusion !== 'success') {
    return { text: 'The last scheduled sync failed', hint: "If it keeps failing, check the access token and the dashboard's server logs.", fix: null };
  }
  // If there has been no run for too long since the last scheduled run after turning on (or since turning on), it has stopped.
  // Whatever the reason, the place to fix it is the Actions tab
  const from = last ? new Date(last.run_started_at).getTime() : enabledAt;
  if (!(Date.now() - from > SYNC_STALL_MS)) return null;
  return {
    text: `Sync hasn't run since ${DATE.format(new Date(from))}`,
    hint: 'Open the Actions tab of your archive repository and check that GitHub Actions is allowed to run the starkive sync workflow.',
    fix: null,
  };
}

/**
 * Describes the last scheduled run in one line.
 *
 * @param {{ run_started_at: string, conclusion: string }|null} last The last finished scheduled run. `null` if there is none
 * @returns {string} Success or failure, then the start time. "Never" if there is none
 */
function describeLastRun(last) {
  if (!last) return 'Never';
  return `${last.conclusion === 'success' ? 'Succeeded' : 'Failed'} · ${DATE_TIME.format(new Date(last.run_started_at))}`;
}

/**
 * Makes the "Add to starkive" bookmarklet link (a bookmark that holds a script instead of an address).
 * Drag it to the bookmarks bar, then press it on a GitHub repository page.
 * It opens this dashboard in a new tab, with that page's address added as `?add=`.
 * `rememberAddParam` takes over from there.
 *
 * @returns {HTMLAnchorElement}
 *
 * NOTE: The bookmarklet runs on a GitHub page.
 * So it only reads the current address and opens a new tab (it doesn't touch the GitHub page content, cookies or tokens).
 * The dashboard address is the address open now (`location.origin`).
 * The session cookie exists only at the canonical address, so this address where Settings is open is the canonical address.
 * This page's CSP blocks running `javascript:` links, so pressing it inside the dashboard does nothing.
 */
function bookmarkletLink() {
  const dashboard = JSON.stringify(`${location.origin}/?add=`); // Safe as a script string even with quotes or backslashes in it
  const code = `(()=>{window.open(${dashboard}+encodeURIComponent(location.href),'_blank')})()`;
  return el('a', {
    href: `javascript:${code}`,
    textContent: 'Add to starkive',
    className: 'bookmarklet',
    title: 'Drag this to your bookmarks bar',
    onclick: (ev) => ev.preventDefault(), // It doesn't run here. Its `title` says how to use it
  });
}

/**
 * Sets the warning mark (dot, `attention` class) and the account button tooltip from `state.sync` and `state.syncError`.
 * The mark goes on Settings in the account menu, and on the account button, so it shows while the menu is closed.
 */
function updateSettingsAttention() {
  const problem = syncProblem(state.sync, state.syncError);
  const button = $('account-button');
  button.classList.toggle('attention', Boolean(problem));
  button.title = problem ? `Account: ${problem.text}. Open Settings in this menu` : 'Account';
  button.setAttribute('aria-label', button.title); // Screen readers hear the problem too
  $('settings-button').classList.toggle('attention', Boolean(problem));
}

/**
 * Opens the Settings dialog, with a sync section and a quick add section (`bookmarkletLink`).
 *
 * NOTE: The switch shows the state the user set (`enabled_since`).
 * Changing it makes the server turn both that setting and the workflow in the DB repository on or off.
 * A change to the workflow in GitHub's Actions tab makes it differ from the setting.
 * So the dialog shows a message and a button to match them (`fix` of `syncProblem`).
 * Scheduled runs and Actions tab changes happen outside the dashboard, so the latest status is fetched again on open.
 * Just opening it writes nothing.
 * One function, `update`, sets locking and display from the current values.
 * The dialog keeps no copy of the status, so a reload of the dashboard (`openDashboard`) and the dialog never disagree.
 * Sync now is drawn from the runs on GitHub (`active_run`), so it shows a running sync after the dialog or the page is opened again.
 */
function openSettings() {
  let busy = false; // `true` while a setting change is sent
  const enabled = el('input', { type: 'checkbox', className: 'switch' });
  enabled.setAttribute('role', 'switch'); // Screen readers then say "on" or "off" instead of "checked"
  enabled.setAttribute('aria-label', 'Sync with GitHub');
  const select = el('select', {}, ...SYNC_INTERVALS.map(([days, label]) => el('option', { value: String(days), textContent: label })));
  const lastRun = el('span'); // The last scheduled run. It links to the run on GitHub when there is one, so it stays on one line
  const problemNote = el('p', { className: 'note' });
  const fixButton = el('button', { type: 'button', className: 'ghost' }); // Button that matches the workflow to the setting when they differ. `fix` of `syncProblem` sets its text and action
  const error = el('p', { className: 'dialog-error', hidden: true });
  const syncNow = el('button', { type: 'button', className: 'ghost', textContent: 'Sync now' });
  const close = el('button', { type: 'button', className: 'ghost', textContent: 'Close' });
  // One line of a section: the name on the left, the control or value on the right
  const row = (name, ...value) => el('div', { className: 'settings-row' }, el('span', { className: 'settings-name', textContent: name }), el('div', { className: 'settings-value' }, ...value));
  const panel = el(
    'div',
    { className: 'dialog-body' },
    el('h2', { textContent: 'Settings' }),
    el(
      'section',
      { className: 'settings-section' },
      el('h3', { textContent: 'Sync' }),
      el('p', { className: 'muted', textContent: "Archives new stars and refreshes repository details and READMEs with GitHub Actions in your archive repository, on a schedule or when you press Sync now. Each run refreshes as many as it has time for, so a large archive is fully refreshed over several runs. A repository deleted before the next sync isn't recorded." }),
      row('Sync with GitHub', enabled),
      row('Schedule', select),
      row('Last scheduled sync', lastRun),
      problemNote,
      fixButton,
      el('div', { className: 'settings-action' }, syncNow),
      error, // Every action that can fail in this dialog is a sync action, so its error shows in this section
    ),
    el(
      'section',
      { className: 'settings-section' },
      el('h3', { textContent: 'Quick add' }),
      el('p', { className: 'muted', textContent: 'Drag this button to your bookmarks bar. On any GitHub repository page, click it to open that repository here in the Add dialog.' }),
      el('div', { className: 'settings-action' }, bookmarkletLink()),
    ),
    el('div', { className: 'actions' }, close),
  );
  // Sets locking, selected values and messages again from the current values (`state.sync`, `state.syncError`, `state.syncRequested`, `syncWatch`, `busy`).
  // Event handlers don't change them directly
  const update = () => {
    const status = state.sync;
    const installed = Boolean(status) && status.workflow !== 'missing';
    const running = state.syncRequested || Boolean(status?.active_run) || Boolean(syncWatch); // `syncWatch` also covers loading the archive again after a run
    const active = status?.workflow === 'active';
    enabled.checked = Boolean(status?.enabled_since);
    enabled.disabled = busy || !installed;
    select.value = String(status?.interval_days ?? 1);
    select.disabled = busy || !enabled.checked;
    syncNow.disabled = busy || running || !active; // GitHub refuses to start a workflow that is off (measured, `core.TriggerSyncWorkflow`)
    syncNow.title = !status ? "Couldn't read the sync status" : active ? '' : 'Sync now needs the sync workflow to be on';
    if (running) syncNow.replaceChildren(spinner(), 'Processing…');
    else syncNow.textContent = 'Sync now';
    const runUrl = safeUrl(status?.last_scheduled_run?.html_url);
    const lastRunText = status ? describeLastRun(status.last_scheduled_run) : 'Unknown';
    if (busy) lastRun.replaceChildren(spinner(), 'Processing…');
    else if (runUrl) lastRun.replaceChildren(el('a', { href: runUrl, target: '_blank', rel: 'noopener noreferrer', title: 'View this run on GitHub', textContent: lastRunText }));
    else lastRun.textContent = lastRunText;
    const problem = syncProblem(status, state.syncError);
    problemNote.hidden = !problem;
    problemNote.textContent = problem ? `${problem.text}. ${problem.hint}` : '';
    fixButton.hidden = !problem?.fix;
    fixButton.textContent = problem?.fix === 'off' ? 'Turn off the workflow' : 'Turn on again';
    fixButton.disabled = busy;
    updateSettingsAttention();
  };
  const showError = (err) => {
    error.textContent = err.message;
    error.hidden = false;
  };

  // Changes a setting and redraws with the latest status the server returns.
  // `body` is `{ enabled }` or `{ sync_interval_days }`.
  // On failure, it redraws with the previous status, so the switch and select box don't drift from the real state
  const change = async (body) => {
    busy = true;
    error.hidden = true;
    update();
    try {
      state.sync = await api('sync-archive', { method: 'PATCH', body });
      state.syncError = ''; // The server returned the status after the change
      syncSettingWrites++; // Status reads sent before this are now outdated
    } catch (err) {
      if (isCancelled(err)) return;
      showError(err);
    }
    busy = false;
    showSyncStatus();
    watchSyncWorkflowRun();
  };
  enabled.onchange = () => change({ enabled: enabled.checked });
  fixButton.onclick = () => change({ enabled: syncProblem(state.sync, state.syncError)?.fix === 'on' });
  select.onchange = () => change({ sync_interval_days: Number(select.value) });

  // Sync now starts a run of the sync workflow, which syncs whatever the interval.
  // `watchSyncWorkflowRun` follows it until it ends
  syncNow.onclick = async () => {
    state.syncRequested = true; // Locks the button at once, so a second press can't start a second run
    error.hidden = true;
    showSyncStatus();
    try {
      const { run } = await api('sync-archive', { method: 'POST' });
      state.sync = { ...state.sync, active_run: run }; // The server answers once the run exists (measured), so it is shown before the next read lists it
    } catch (err) {
      if (!isCancelled(err)) showError(err);
    } finally {
      state.syncRequested = false;
    }
    showSyncStatus();
    watchSyncWorkflowRun();
  };

  syncStatusViews.add(update);
  const modal = openModal({ panel, className: 'settings-dialog', onClose: () => syncStatusViews.delete(update) });
  close.onclick = modal.close;
  update();
  // First draw with the status already held.
  // Redraw when the latest status arrives (it may have changed outside the dashboard).
  // If the session closes meanwhile (sign-out, or a reload opening a new session), the request is cancelled and nothing changes
  fetchSyncStatus()
    .then((result) => {
      // A read sent before Sync now was pressed could hide the run it started, so it is dropped.
      // `watchSyncWorkflowRun` reads the status again while a run goes.
      // A read sent before a setting change could undo it, so it is dropped too
      if (state.syncRequested || syncWatch || isSyncStatusOutdated(result)) return;
      ({ sync: state.sync, syncError: state.syncError } = result);
      showSyncStatus();
      watchSyncWorkflowRun();
    })
    .catch(() => {}); // Only a cancel error comes here
}

// Redraw functions of the open Settings dialogs.
// `showSyncStatus` calls them whenever `state.sync` changes
const syncStatusViews = new Set();

/** Draws the sync status again: the warning mark (`updateSettingsAttention`) and every open Settings dialog. */
function showSyncStatus() {
  updateSettingsAttention();
  for (const update of syncStatusViews) update();
}

// Time between reads of the sync status while a sync runs (milliseconds).
// One read is about four GitHub requests on the server
const SYNC_WATCH_MS = 5000;

// Time between reads once a run has been followed for `SYNC_WATCH_SLOW_AFTER_MS` (milliseconds).
// A run waiting in the queue for long (a GitHub outage) then doesn't use up the rate limit
const SYNC_WATCH_SLOW_MS = 30 * 1000;

// Time after which a followed run is read every `SYNC_WATCH_SLOW_MS` (milliseconds).
// A sync call ends on the server within 290 seconds (`requestTimeLimit` in `core`), so a run past this mostly waits in the queue
const SYNC_WATCH_SLOW_AFTER_MS = 5 * 60 * 1000;

let syncWatch = null; // The loop of `watchSyncWorkflowRun` while it runs, including loading the archive again after a run. `null` otherwise

const announcedSyncRunIds = new Set(); // IDs of runs started by hand whose end was reported, so a stale read can't report one twice

/**
 * Follows the running syncs (`state.sync.active_run`) one after another until none runs (`followSyncWorkflowRun`).
 * It does nothing if no sync runs or it already follows them.
 *
 * NOTE: Call it whenever `state.sync` is set, so a sync that started elsewhere (a scheduled run, another device) is followed too.
 * It keeps going after the Settings dialog closes, and stops when the session closes.
 * A run waiting behind the one followed (a scheduled run behind Sync now) is followed next, so the status doesn't stop at the first run.
 * Sync now stays locked (`syncWatch`) until the archive is loaded again and the end is reported.
 * A press before that would compare the next run with the archive from before this run, and count its new items twice.
 */
function watchSyncWorkflowRun() {
  if (syncWatch || !state.sync?.active_run) return;
  const signal = sessionScope?.signal;
  syncWatch = (async () => {
    try {
      while (state.sync?.active_run) await followSyncWorkflowRun(state.sync.active_run, signal);
    } catch (err) {
      if (!isCancelled(err)) throw err;
    } finally {
      syncWatch = null;
      showSyncStatus(); // Unlocks Sync now
    }
  })();
}

/**
 * Follows the sync run `watched` until it ends, reading the status every `SYNC_WATCH_MS` (later `SYNC_WATCH_SLOW_MS`).
 * When a run started by hand (Sync now, or Run workflow in the Actions tab) ends, it loads the archive again and reports the new items, the failure or the cancel.
 *
 * @param {object} watched The run (`core.SyncWorkflowRun`)
 * @param {AbortSignal} signal This session's cancel signal
 * @returns {Promise<void>} Resolves once the run ended and its end was reported. `state.sync` then holds the latest status
 * @throws {DOMException} `AbortError` if the session closed (`isCancelled`)
 *
 * NOTE: It follows one run by its ID, so a run that starts right after it doesn't take its place.
 * The end is judged only from its own reads.
 * A failed read says nothing about the run, so the status is read again later.
 * The archive is loaded again in the same session (`loadAll`), not with `openDashboard`.
 * A new session would cancel the requests of open dialogs, which then stay locked.
 */
async function followSyncWorkflowRun(watched, signal) {
  const idsBefore = new Set(state.starItems.map((item) => item.id));
  const startedAt = Date.now();
  for (;;) {
    await wait(Date.now() - startedAt < SYNC_WATCH_SLOW_AFTER_MS ? SYNC_WATCH_MS : SYNC_WATCH_SLOW_MS, signal);
    const result = await fetchSyncStatus();
    // A read sent before a setting change may hold the old setting, so it is skipped like a failed read
    if (result.syncError || isSyncStatusOutdated(result)) continue;
    state.sync = result.sync;
    state.syncError = '';
    showSyncStatus();
    // The run ended when it is the last finished run, or when nothing runs any more
    if (result.sync.last_run?.id === watched.id || !result.sync.active_run) break;
  }
  if (watched.event !== 'workflow_dispatch') return; // A scheduled run only redraws the status
  if (announcedSyncRunIds.has(watched.id)) return;
  announcedSyncRunIds.add(watched.id);
  try {
    await loadAll(signal);
    render();
    liveMetaLoader.reportErrors = true;
  } catch (err) {
    if (isCancelled(err)) throw err; // A closed session already told the user, so nothing is written over it
    setStatus(err.message, { error: true });
    return;
  }
  const finished = state.sync.last_run?.id === watched.id ? state.sync.last_run : null; // `null` if another run finished after it before a read saw it
  // The run ran on GitHub, so its counts aren't here.
  // The new items are found by comparing the archive before and after
  const newCount = state.starItems.filter((item) => !idsBefore.has(item.id)).length;
  if (finished?.conclusion === 'cancelled') setStatus('Sync was cancelled');
  else if (finished && finished.conclusion !== 'success') setStatus('Sync failed. See the run in the Actions tab of your archive repository', { error: true });
  else setStatus(`${finished ? 'Synced' : 'Sync finished'}: ${newCount} new`);
}

/**
 * Waits `ms` milliseconds.
 *
 * @param {number} ms Time to wait (milliseconds)
 * @param {AbortSignal} [signal] If it aborts, the wait ends at once with its reason (an `AbortError`, `isCancelled`)
 * @returns {Promise<void>}
 */
function wait(ms, signal) {
  return new Promise((resolve, reject) => {
    signal?.throwIfAborted();
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal.reason);
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort); // A long follow makes many waits, so each removes its listener
      resolve();
    }, ms);
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

// ---------------------------------------------------------------------------
// Page flow
// ---------------------------------------------------------------------------

let statusTimer; // Timer that clears the status line on its own

/**
 * Shows a message in the status line at the bottom of the page.
 * Errors stay longer, so the user can read them.
 *
 * @param {string} message The text to show. An empty string hides the status line
 * @param {{ error?: boolean, sticky?: boolean, busy?: boolean }} [options] `error`: shown as an error for 8 seconds (otherwise 3 seconds).
 *   `sticky`: not cleared on its own.
 *   `busy`: work in progress. It shows the progress indicator and isn't cleared on its own (the next message replaces it)
 */
function setStatus(message, { error = false, sticky = false, busy = false } = {}) {
  const box = $('status');
  box.textContent = message;
  if (busy && message) box.prepend(spinner());
  box.classList.toggle('error', error);
  box.classList.toggle('visible', Boolean(message));
  clearTimeout(statusTimer);
  if (message && !sticky && !busy) statusTimer = setTimeout(() => setStatus(''), error ? 8000 : 3000);
}

/**
 * Runs async work with progress, done and error messages.
 *
 * @param {() => Promise<void>} task The work to do
 * @param {string} pending Progress message ("…" is added after it)
 * @param {string} done Message to show on success
 * @returns {Promise<boolean>} `true` on success.
 *   On failure, it shows the error message and returns `false` (work cancelled because the session closed is not reported)
 */
async function run(task, pending, done) {
  setStatus(`${pending}…`, { busy: true });
  try {
    await task();
    setStatus(done);
    return true;
  } catch (err) {
    if (!isCancelled(err)) setStatus(err.message, { error: true });
    return false;
  }
}

/**
 * Shows the sign-in screen.
 * It hides the dashboard and its top bar menus, and focuses the sign-in button.
 */
function showLogin() {
  closeMenu();
  document.title = 'Starkive';
  $('title-detail').hidden = true;
  $('login').hidden = false;
  $('dashboard').hidden = true;
  for (const id of ['add-menu', 'account-menu']) $(id).hidden = true;
  $('login-button').disabled = false;
  $('login-button').focus();
}

// Message for each reason when GitHub sign-in fails and the server sends the browser back to `/?login_error=<reason>`.
// The reason values are the server's `core.GitHubLoginFailure`
const LOGIN_ERRORS = {
  cancelled: 'Sign-in was cancelled on GitHub',
  expired: 'Sign-in took too long or was interrupted. Try again',
  not_allowed: 'This GitHub account is not allowed to use this dashboard',
  failed: "Couldn't sign in with GitHub. Try again",
};

/**
 * Reads `login_error` from the address and turns it into a message.
 * It removes it from the address, so a reload doesn't show it again.
 *
 * @returns {string} The message. An empty string if there is no `login_error`, and the general failure message for an unknown reason
 */
function takeRedirectError() {
  const params = new URLSearchParams(location.search);
  const reason = params.get('login_error');
  if (reason === null) return '';
  params.delete('login_error');
  const rest = params.toString();
  history.replaceState(null, '', location.pathname + (rest ? `?${rest}` : ''));
  return LOGIN_ERRORS[reason] ?? LOGIN_ERRORS.failed;
}

const PENDING_ADD_KEY = 'starkive.pendingAdd'; // `sessionStorage` key that remembers the repository to add in this tab
const PENDING_ADD_TTL = 10 * 60 * 1000; // How long the remembered value can be used (milliseconds). Same as the lifetime of the sign-in state cookie
const PENDING_ADD_MAX_LENGTH = 300; // Max length of the value. Enough for a repository address. Values that are way too long are dropped

/**
 * Remembers the address's `add` parameter (the repository to add, owner/repo or a GitHub address) in this tab.
 * It removes it from the address, so a reload doesn't open it again.
 * When the dashboard loads the archive, `readPendingAdd` takes it out and opens the Add dialog with it filled in (`openDashboard`).
 *
 * NOTE: It is remembered in the tab (`sessionStorage`) instead of used from the address because of sign-in.
 * Before sign-in, the value in the address is lost during the trip to GitHub (the callback always returns to `/`).
 * But `sessionStorage` of the same tab stays.
 * Anyone can make a link with this value, so it only fills the dialog and never saves.
 * If it saved on its own, just clicking someone else's link would make my dashboard page send a request,
 * and a commit would pass the `Origin` check.
 * Where storage is blocked (private mode and so on), only the value is lost.
 */
function rememberAddParam() {
  const params = new URLSearchParams(location.search);
  const value = params.get('add')?.trim();
  if (value === undefined) return;
  params.delete('add');
  const rest = params.toString();
  history.replaceState(null, '', location.pathname + (rest ? `?${rest}` : ''));
  // Accept only repository shapes (owner/repo or a GitHub address).
  // This value is shown as is on the sign-in screen, so someone else's link can't show any text it likes.
  // The server does the real parsing and checks after Add is pressed
  const looksLikeRepo = (value.startsWith('https://github.com/') && !/\s/.test(value)) || /^[\w.-]+\/[\w.-]+$/.test(value);
  if (!looksLikeRepo || value.length > PENDING_ADD_MAX_LENGTH) return;
  try {
    sessionStorage.setItem(PENDING_ADD_KEY, JSON.stringify({ value, at: Date.now() }));
  } catch {
    // If storage can't be used, don't remember it
  }
}

/**
 * Reads the repository to add that was remembered in this tab.
 * If it is past its time limit (`PENDING_ADD_TTL`) or can't be read, it is deleted.
 *
 * @param {boolean} consume If `true`, it is deleted after reading. If `false`, it stays for a later read
 * @returns {string} The repository to add. An empty string if there is none
 */
function readPendingAdd(consume) {
  try {
    const raw = sessionStorage.getItem(PENDING_ADD_KEY);
    if (raw === null) return '';
    let pending = null;
    try {
      pending = JSON.parse(raw);
    } catch {
      // A value that can't be read is deleted below
    }
    const fresh = typeof pending?.value === 'string' && typeof pending.at === 'number' && Date.now() - pending.at < PENDING_ADD_TTL;
    if (consume || !fresh) sessionStorage.removeItem(PENDING_ADD_KEY);
    return fresh ? pending.value : '';
  } catch {
    return ''; // Storage can't be used here
  }
}

/**
 * Moves to the dashboard's canonical address if the current address is not it.
 *
 * @param {unknown} dashboardURL The canonical address the server sent with the 409 for sign-in start (`dashboard_url`, the address registered in the GitHub App)
 * @returns {boolean} `true` if it moved.
 *   `false` if there is no address, it is neither https nor a local dev server address (`http://localhost`, `http://127.0.0.1`), or it is already that address
 *
 * NOTE: GitHub sends the sign-in result back to the canonical address.
 * The state cookie set at the start belongs only to the address where sign-in started.
 * So starting at another address (like another domain Vercel gives) always fails.
 * That is why the server doesn't start, and tells the canonical address instead.
 * The session cookie also exists only at the canonical address.
 * So someone who came in through another address is always moved here when they press sign-in.
 */
function moveToDashboardURL(dashboardURL) {
  let target;
  try {
    target = new URL(dashboardURL);
  } catch {
    return false;
  }
  const local = target.protocol === 'http:' && ['localhost', '127.0.0.1'].includes(target.hostname);
  if ((target.protocol !== 'https:' && !local) || target.origin === location.origin) return false;
  location.replace(`${target.origin}/`); // Move to the first page of the canonical address. The user presses sign-in again there
  return true;
}

/**
 * Opens a new session, loads the data and opens the dashboard.
 *
 * @param {string} repo DB repository name (`owner/repo`)
 * @param {string} [done] Message to show after loading, instead of "Loaded N repositories"
 * @param {{ signal: AbortSignal, archive: Promise, sync: Promise }} [preloaded] The session `start` already opened when the page opened,
 *   and the archive and sync status requests sent in that session. If missing, a new session is opened and the requests are sent now
 *
 * NOTE: Opening a new session cancels the previous session's loading, so it doesn't overwrite this page.
 * After loading, the status line shows only the first of these:
 * an error from failed live details, a sync problem, or the done message.
 */
async function openDashboard(repo, done = '', preloaded = null) {
  const signal = preloaded?.signal ?? beginSession();
  const syncRequest = preloaded?.sync ?? fetchSyncStatus(); // Sent after the new session opens, so it carries that session's signal
  syncRequest.catch(() => {}); // Rejects only when cancelled. If loading fails first, nobody awaits it, so this keeps it out of the console
  state.repo = repo;
  const link = $('repo-link');
  const url = safeUrl(`https://github.com/${repo}`);
  link.hidden = !url;
  if (url) link.href = url;
  $('repo-name').textContent = repo;
  $('login').hidden = true;
  $('dashboard').hidden = false;
  for (const id of ['add-menu', 'account-menu']) $(id).hidden = false;
  updateSettingsAttention();

  setStatus('Loading…', { busy: true });
  try {
    await loadAll(signal, preloaded?.archive);
    render();
    // If the user came with a repository to add, open the Add dialog with it filled in.
    // The star list definitions are loaded by now, so the select box gets filled
    const pendingAdd = readPendingAdd(true);
    if (pendingAdd) openAddRepo(pendingAdd);
    // Wait only for the live details of items drawn on the first screen.
    // After that, workers report failed batches right away
    setStatus('Loading details from GitHub…', { busy: true });
    await waitForLiveMetaIdle(liveMetaLoader);
    const syncResult = await syncRequest;
    signal.throwIfAborted();
    // A Sync now pressed or a setting changed in Settings while the page loaded is newer than this read
    if (!state.syncRequested && !syncWatch && !isSyncStatusOutdated(syncResult)) ({ sync: state.sync, syncError: state.syncError } = syncResult);
    showSyncStatus();
    watchSyncWorkflowRun(); // A sync may be running, started before the page opened or on another device
    const metaError = liveMetaLoader.error;
    liveMetaLoader.reportErrors = true;
    const syncTrouble = syncProblem(state.sync, state.syncError);
    if (metaError) setStatus(`Could not load some repository details from GitHub: ${metaError.message}`, { error: true });
    else if (syncTrouble) setStatus(`${syncTrouble.text}. Open Settings for details`, { error: true }); // Warn, so a stopped sync doesn't go unnoticed
    else setStatus(done || `Loaded ${state.starItems.length} repositories`);
  } catch (err) {
    if (!isCancelled(err)) setStatus(err.message, { error: true });
  }
}

/**
 * Opens the dashboard if the session is alive, or the sign-in screen otherwise.
 * If the user came back after a failed GitHub sign-in, it shows the reason.
 * A 401 before sign-in is normal, so it is not reported as an error.
 * If the address has a repository to add (`add`), it is remembered.
 * Before sign-in, a message says it will be added after signing in.
 *
 * NOTE: It requests the sign-in state (`login-session`), the three archive files and the sync status at the same time.
 * Requesting the rest only after the sign-in state arrives would make the page appear that much later.
 * Before sign-in, those other requests end with 401.
 * So they are sent with `beforeSignIn`, which keeps them from counting as session expiry, and their results are dropped.
 */
async function start() {
  const redirectError = takeRedirectError();
  rememberAddParam();
  const signal = beginSession();
  const archive = fetchArchiveFiles(true);
  archive.catch(() => {}); // Fails before sign-in. This keeps it from showing in the console as an unhandled rejection (`loadAll` awaits it separately)
  const sync = fetchSyncStatus(true);
  sync.catch(() => {}); // Rejects only when cancelled below. `openDashboard` awaits it separately
  let repo;
  let username;
  try {
    ({ repo, username } = await api('login-session'));
  } catch (err) {
    sessionScope?.abort(); // Stop the requests sent early
    sessionScope = null;
    showLogin();
    const message = redirectError || (err.status === 401 ? '' : err.message);
    const pendingAdd = readPendingAdd(false);
    if (message) setStatus(message, { error: true });
    else if (pendingAdd) setStatus(`Sign in to add ${pendingAdd}`, { sticky: true });
    return;
  }
  showAccount(username);
  await openDashboard(repo, '', { signal, archive, sync });
}

/**
 * Fills the account menu with the `username` of the account that signed in, and puts it in the tab title and the top bar title.
 * The username and the profile link are hidden if the username is empty (it couldn't be read),
 * and the profile link also if it doesn't make a GitHub address (`safeUrl`).
 */
function showAccount(username) {
  const name = $('account-username');
  name.textContent = username;
  name.hidden = !username;
  document.title = username ? `Starkive (${username})` : 'Starkive';
  $('title-detail').textContent = `/ ${username}`;
  $('title-detail').hidden = !username;
  const link = $('profile-link');
  const url = username ? safeUrl(`https://github.com/${encodeURIComponent(username)}`) : null;
  link.hidden = !url;
  if (url) link.href = url;
}

/**
 * Fills in the icons (`data-icon`) and attaches handlers to the fixed page elements (top bar menus, search and filters, "Load more").
 * Call it only once, because a second call attaches every handler twice.
 */
function bindEvents() {
  for (const slot of document.querySelectorAll('[data-icon]')) slot.replaceWith(icon(slot.dataset.icon));
  for (const menu of document.querySelectorAll('.menu')) bindMenu(menu);
  // A press outside the open menu closes it
  document.addEventListener('pointerdown', (ev) => {
    if (openMenu && !openMenu.contains(ev.target)) closeMenu();
  });
  // Esc closes the open menu, even when the focus isn't in it.
  // If the focus was in the menu, it goes back to the menu's button, so keyboard users don't lose their place
  document.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Escape' || !openMenu) return;
    const button = openMenu.querySelector('.menu-button');
    const focusWasInMenu = openMenu.contains(document.activeElement);
    ev.preventDefault();
    closeMenu();
    if (focusWasInMenu) button.focus();
  });

  // Sign-in start: go to the GitHub authorization address the server gives.
  // After approval, GitHub sends the browser back to this page through the server callback.
  // The address is followed only if it starts with `https://github.com/` (`safeUrl`)
  $('login-button').onclick = async () => {
    const button = $('login-button');
    button.disabled = true;
    setStatus('Redirecting to GitHub…', { busy: true });
    try {
      const url = safeUrl((await api('login-session', { method: 'POST' })).url);
      if (!url) throw new Error("Couldn't start GitHub sign-in");
      location.assign(url);
    } catch (err) {
      button.disabled = false;
      // 409: if sign-in started somewhere other than the canonical address, move there.
      // Otherwise (the setup tool needs to be run), show the server's message
      if (!(err.status === 409 && moveToDashboardURL(err.body?.dashboard_url))) setStatus(err.message, { error: true });
    }
  };

  // Sign-out closes the page only after the server says it deleted the cookie.
  // Closing on failure would look signed out while the cookie stays
  $('logout').onclick = async () => {
    try {
      await api('login-session', { method: 'DELETE' });
    } catch (err) {
      setStatus(`Couldn't sign out: ${err.message}`, { error: true });
      return;
    }
    endSession('Signed out');
  };

  // Going back from the GitHub page can make the browser restore this page from memory as it was (back/forward cache).
  // Then the locked button and "Redirecting to GitHub…" from just before the move remain.
  // So the sign-in screen state is reset when the page is restored
  window.addEventListener('pageshow', (ev) => {
    if (!ev.persisted || $('login').hidden) return;
    $('login-button').disabled = false;
    setStatus('');
  });

  $('import-button').onclick = openImport;

  $('settings-button').onclick = openSettings;

  $('add-button').onclick = () => openAddRepo(); // Wrapped, so the click event doesn't land in the `prefill` argument

  // Search applies after typing stops for 150 ms.
  // This avoids a redraw for every character
  let searchTimer;
  $('search').oninput = (ev) => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => setFilter('query', ev.target.value), 150);
  };
  for (const key of ['language', 'sort']) {
    $(key).onchange = (ev) => setFilter(key, ev.target.value);
  }

  const showMore = () => {
    state.shown += PAGE_SIZE;
    render();
  };
  $('more').onclick = showMore;
  // Infinite scroll: when the "Load more" button comes within 600px below the screen, the next batch is drawn early.
  // The button stays, for keyboard users and for when `IntersectionObserver` doesn't work.
  // If the button is still visible after drawing (a tall screen), the observer doesn't report again.
  // So the observation is set again in the next frame to check once more
  const observer = new IntersectionObserver(
    (entries) => {
      if (!entries[0].isIntersecting || $('more').hidden) return;
      showMore();
      requestAnimationFrame(() => {
        observer.unobserve($('more'));
        observer.observe($('more'));
      });
    },
    { rootMargin: '0px 0px 600px 0px' },
  );
  observer.observe($('more'));

  $('reload').onclick = () => openDashboard(state.repo);

  // Pressing `/` moves to the search box. A `/` typed in an input is ignored
  document.addEventListener('keydown', (ev) => {
    if (ev.key !== '/' || ev.target.closest('input, textarea, select')) return;
    ev.preventDefault();
    $('search').focus();
  });
}

// When the page opens: attach handlers, and open the screen that matches the sign-in state
bindEvents();
start();
