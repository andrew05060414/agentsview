import { allowedPath } from "./paths.js";
import { platforms } from "./platforms.js";

// Covers message encoding and request shapes; bump both peers together.
const version = 3;

async function loadedTab(id) {
  await new Promise((resolve, reject) => {
    const updated = (tabId, change) => {
      if (tabId === id && change.status === "complete") finish();
    };
    const removed = (tabId) => {
      if (tabId === id) finish(new Error("Browser tab closed"));
    };
    const timer = setTimeout(() => finish(new Error("Browser tab did not load")), 45000);
    function finish(error) {
      clearTimeout(timer);
      chrome.tabs.onUpdated.removeListener(updated);
      chrome.tabs.onRemoved.removeListener(removed);
      if (error) reject(error);
      else resolve();
    }
    chrome.tabs.onUpdated.addListener(updated);
    chrome.tabs.onRemoved.addListener(removed);
    chrome.tabs.get(id).then((tab) => {
      if (tab.status === "complete") finish();
    }, finish);
  });
}

// Sync sends several requests at once, one stream per site. Requests for the
// same site share one background tab that this worker opens, and the last of
// them to finish closes it. A tab the user already has open is reused and
// never closed. Before this, every request could open a fresh tab and none
// were ever closed: the orphans accumulated until Chrome discarded tabs
// mid-request, which surfaced as "Frame with ID 0 was removed" and
// "No tab with id" failures.
const owned = new Map(); // platform -> { tab: Promise<Tab>, users: number }

// Tabs that died mid-request. The next acquire skips them so a retry does not
// race Chrome and pick the same closing or discarded tab again.
const deadTabs = new Set();

async function acquireTab(platform, config) {
  const share = (entry) => {
    entry.users++;
    return { entry, tab: entry.tab };
  };
  const entry = owned.get(platform);
  if (entry) return share(entry);
  const [existing] = (await chrome.tabs.query({ url: `${config.origin}/*`, discarded: false }))
    .filter((tab) => !deadTabs.has(tab.id));
  if (existing) return { entry: undefined, tab: Promise.resolve(existing) };
  // Another request may have started opening the tab while we were querying.
  const raced = owned.get(platform);
  if (raced) return share(raced);
  const created = { users: 0 };
  created.tab = chrome.tabs
    .create({ url: config.newTabURL, active: false })
    .then((tab) => tab, (error) => {
      if (owned.get(platform) === created) owned.delete(platform);
      throw error;
    });
  owned.set(platform, created);
  return share(created);
}

function releaseTab(platform, entry) {
  if (!entry || --entry.users > 0) return;
  if (owned.get(platform) === entry) owned.delete(platform);
  entry.tab.then(
    (tab) => chrome.tabs.remove(tab.id).catch(() => {}),
    () => {},
  );
}

// tabGone reports the errors Chrome raises when the tab a request was using
// disappeared mid-request: the user closed it, or the browser discarded or
// crashed its renderer.
function tabGone(error) {
  const message = String(error?.message ?? error);
  return (
    message.includes("No tab with id") ||
    message.includes("Frame with ID") ||
    message.includes("Browser tab closed")
  );
}

async function fetchOnTab({ id, platform, path }, config) {
  // A tab Chrome killed between requests leaves no reason to fail the run:
  // drop it and retry once on a fresh one.
  for (let attempt = 0; ; attempt++) {
    const claim = await acquireTab(platform, config);
    let tabId;
    try {
      const tab = await claim.tab;
      tabId = tab.id;
      await loadedTab(tabId);
      const target = { tabId };
      await chrome.scripting.executeScript({ target, files: config.scripts, world: "ISOLATED" });
      const [reply] = await chrome.scripting.executeScript({
        target,
        world: "ISOLATED",
        func: (name, url, headers) =>
          typeof globalThis[name] === "function" ? globalThis[name](url, headers) : undefined,
        args: [config.fetchFunction, `${config.origin}${path}`, config.headers ?? {}],
      });
      if (typeof reply?.result?.status !== "number")
        throw new Error("The page changed during Sync; try Sync again");
      return { ...reply.result, id, version };
    } catch (error) {
      if (attempt === 0 && tabGone(error)) {
        if (tabId !== undefined) deadTabs.add(tabId);
        if (claim.entry && owned.get(platform) === claim.entry) owned.delete(platform);
        continue;
      }
      throw error;
    } finally {
      releaseTab(platform, claim.entry);
    }
  }
}

async function request({ id, platform, path }, port) {
  let result;
  try {
    if (!Object.hasOwn(platforms, platform)) {
      // An older extension than the server: the server reports that the
      // extension needs an update without dropping the connection.
      result = { id, version, status: 0, unsupported: true };
    } else {
      const config = platforms[platform];
      if (!(await allowedPath(platform, path))) throw new Error("Unsupported browser fetch path");
      result = await fetchOnTab({ id, platform, path }, config);
    }
  } catch (error) {
    result = { id, version, status: 0, error: error.message };
  }
  if (new TextEncoder().encode(JSON.stringify(result)).byteLength > 64 * 1024 * 1024)
    result = { id, version, status: 413 };
  try {
    port.postMessage(result);
  } catch {
    /* Disconnect fails the server's pending request. */
  }
}

let connection;
function connect() {
  if (connection) return;
  const port = chrome.runtime.connectNative("io.kenn.agentsview");
  connection = port;
  port.onMessage.addListener((message) => {
    void request(message, port);
  });
  port.onDisconnect.addListener(() => {
    void chrome.runtime.lastError;
    connection = undefined;
    setTimeout(connect, 5000);
  });
}
chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
connect();
