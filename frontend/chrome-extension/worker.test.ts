import { readFileSync } from "node:fs";
import { afterEach, beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { platforms } from "./platforms.js";

let message: (request: object) => void;
let disconnect: () => void;
const port = {
  postMessage: vi.fn(),
  onMessage: {
    addListener: vi.fn((callback) => {
      message = callback;
    }),
  },
  onDisconnect: {
    addListener: vi.fn((callback) => {
      disconnect = callback;
    }),
  },
};
const chrome = {
  runtime: {
    getURL: vi.fn((file: string) => `chrome-extension://test/${file}`),
    connectNative: vi.fn(() => port),
    onStartup: { addListener: vi.fn() },
    onInstalled: { addListener: vi.fn() },
  },
  tabs: {
    query: vi.fn(),
    create: vi.fn(),
    get: vi.fn(),
    remove: vi.fn(async () => {}),
    onUpdated: { addListener: vi.fn(), removeListener: vi.fn() },
    onRemoved: { addListener: vi.fn(), removeListener: vi.fn() },
  },
  scripting: { executeScript: vi.fn() },
};
beforeEach(async () => {
  vi.resetModules();
  vi.clearAllMocks();
  vi.stubGlobal("chrome", chrome);
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async (url: string) =>
        new Response(
          readFileSync(
            `../internal/importer/browser_requests/${url.replace("chrome-extension://test/requests/", "")}`,
            "utf8",
          ),
        ),
    ),
  );
  chrome.tabs.query.mockResolvedValue([{ id: 7 }]);
  chrome.tabs.get.mockResolvedValue({ id: 7, status: "complete" });
  chrome.tabs.create.mockResolvedValue({ id: 8 });
  chrome.scripting.executeScript.mockImplementation(async (options) =>
    options.args ? [{ result: { status: 200, body: "chats" } }] : [{ result: true }],
  );
  await import("./worker.js");
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

async function request(path = "/api/organizations") {
  message({ id: "a", platform: "claude-ai", path, version: 3 });
  await vi.waitFor(() => expect(port.postMessage).toHaveBeenCalledOnce());
  return port.postMessage.mock.calls[0]![0];
}

describe("Chrome native host worker", () => {
  it("refuses unsupported paths before touching tabs", async () => {
    message({ id: "a", platform: "claude-ai", path: "/api/settings", version: 3 });
    await vi.waitFor(() => expect(port.postMessage).toHaveBeenCalledOnce());
    expect(port.postMessage).toHaveBeenCalledWith({
      id: "a",
      version: 3,
      status: 0,
      error: "Unsupported browser fetch path",
    });
    expect(chrome.tabs.query).not.toHaveBeenCalled();
    expect(chrome.tabs.create).not.toHaveBeenCalled();
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
  });

  it.each([undefined, "", "codex", "__proto__", "toString"])(
    "replies unsupported for platform %s without touching tabs",
    async (platform) => {
      message({ id: "a", platform, path: "/api/organizations", version: 3 });
      await vi.waitFor(() => expect(port.postMessage).toHaveBeenCalledOnce());
      expect(port.postMessage).toHaveBeenCalledWith({
        id: "a",
        version: 3,
        status: 0,
        unsupported: true,
      });
      expect(chrome.tabs.query).not.toHaveBeenCalled();
      expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
    },
  );

  it.each([undefined, 0, 1, 3])(
    "stamps its own version on replies to peer version %s",
    async (version) => {
      message({ id: "a", platform: "claude-ai", path: "/api/organizations", version });
      await vi.waitFor(() => expect(port.postMessage).toHaveBeenCalledOnce());
      expect(port.postMessage).toHaveBeenCalledWith({
        id: "a",
        version: 3,
        status: 200,
        body: "chats",
      });
    },
  );

  it.each(Object.entries(platforms))(
    "fetches %s paths in an existing tab and replies by id",
    async (platform, config) => {
      const [path] = readFileSync(
        `../internal/importer/testdata/browser_paths/${platform}.txt`,
        "utf8",
      ).split("\n");
      message({ id: "a", platform, path, version: 3 });
      await vi.waitFor(() => expect(port.postMessage).toHaveBeenCalledOnce());
      expect(port.postMessage).toHaveBeenCalledWith({
        id: "a",
        version: 3,
        status: 200,
        body: "chats",
      });
      expect(chrome.runtime.connectNative).toHaveBeenCalledExactlyOnceWith("io.kenn.agentsview");
      expect(chrome.tabs.query).toHaveBeenCalledWith({
        url: `${config.origin}/*`,
        discarded: false,
      });
      expect(chrome.scripting.executeScript).toHaveBeenNthCalledWith(1, {
        target: { tabId: 7 },
        files: config.scripts,
        world: "ISOLATED",
      });
      expect(chrome.scripting.executeScript).toHaveBeenLastCalledWith({
        target: { tabId: 7 },
        world: "ISOLATED",
        func: expect.any(Function),
        args: [
          config.fetchFunction,
          `${config.origin}${path}`,
          ("headers" in config ? config.headers : undefined) ?? {},
        ],
      });
      expect(chrome.tabs.remove).not.toHaveBeenCalled();
    },
  );

  it("injects pageFetch into a fresh tab and closes it afterwards", async () => {
    const pageFetch = vi.fn(() => ({ status: 200, body: "chats" }));
    chrome.scripting.executeScript.mockImplementation(async (options) => {
      if (options.files) {
        vi.stubGlobal("pageFetch", pageFetch);
        return [];
      }
      return [{ result: (await options.func(...(options.args ?? []))) ?? null }];
    });
    chrome.tabs.query.mockResolvedValue([]);
    expect(await request()).toEqual({ id: "a", version: 3, status: 200, body: "chats" });
    expect(chrome.tabs.create).toHaveBeenCalledWith({
      url: "https://claude.ai/new",
      active: false,
    });
    await vi.waitFor(() => expect(chrome.tabs.remove).toHaveBeenCalledExactlyOnceWith(8));
    expect(chrome.scripting.executeScript).toHaveBeenNthCalledWith(1, {
      target: { tabId: 8 },
      files: ["page_fetch.js"],
      world: "ISOLATED",
    });
    expect(pageFetch).toHaveBeenCalledExactlyOnceWith("https://claude.ai/api/organizations", {});
  });

  it("shares one background tab per site across concurrent requests and closes each once", async () => {
    chrome.tabs.query.mockResolvedValue([]);
    chrome.tabs.create.mockImplementation(async ({ url }: { url: string }) => ({
      id: url.includes("claude") ? 8 : 9,
    }));
    chrome.tabs.get.mockImplementation(async (id: number) => ({ id, status: "complete" }));
    for (const id of ["a", "b", "c"])
      message({ id, platform: "claude-ai", path: "/api/organizations", version: 3 });
    for (const id of ["d", "e"])
      message({ id, platform: "claude-ai", path: "/api/organizations", version: 3 });
    message({
      id: "f",
      platform: "grok-web",
      path: "/rest/app-chat/conversations?pageSize=100",
      version: 3,
    });
    await vi.waitFor(() => expect(port.postMessage).toHaveBeenCalledTimes(6));
    expect(chrome.tabs.create).toHaveBeenCalledTimes(2);
    await vi.waitFor(() => expect(chrome.tabs.remove).toHaveBeenCalledTimes(2));
    expect(chrome.tabs.remove).toHaveBeenCalledWith(8);
    expect(chrome.tabs.remove).toHaveBeenCalledWith(9);
  });

  it("retries once on a fresh tab when the tab dies mid-request", async () => {
    chrome.tabs.query.mockResolvedValue([]);
    chrome.tabs.create
      .mockResolvedValueOnce({ id: 8 })
      .mockResolvedValueOnce({ id: 9 });
    chrome.tabs.get.mockImplementation(async (id: number) => ({ id, status: "complete" }));
    let injected = 0;
    chrome.scripting.executeScript.mockImplementation(async (options) => {
      if (options.files) {
        injected += 1;
        if (injected === 1) throw new Error("Frame with ID 0 was removed");
        return [{ result: true }];
      }
      return [{ result: { status: 200, body: "chats" } }];
    });
    expect(await request()).toEqual({ id: "a", version: 3, status: 200, body: "chats" });
    expect(chrome.tabs.create).toHaveBeenCalledTimes(2);
  });

  it("skips a dead tab the query still returns on the retry", async () => {
    // The first attempt reuses the user's tab; it dies mid-request. The retry's
    // query still lists it briefly, so it must be filtered out.
    chrome.tabs.query.mockResolvedValue([{ id: 7 }]);
    chrome.tabs.get.mockImplementation(async (id: number) => ({ id, status: "complete" }));
    chrome.tabs.create.mockResolvedValue({ id: 8 });
    let injected = 0;
    chrome.scripting.executeScript.mockImplementation(async (options) => {
      if (options.files) {
        injected += 1;
        if (options.target.tabId === 7) throw new Error("No tab with id: 7");
        return [{ result: true }];
      }
      return [{ result: { status: 200, body: "chats" } }];
    });
    expect(await request()).toEqual({ id: "a", version: 3, status: 200, body: "chats" });
    expect(injected).toBe(2);
    expect(chrome.tabs.create).toHaveBeenCalledOnce();
    // The dead tab belonged to the user: it is never closed by the worker.
    // The tab the retry created is the worker's own and is closed.
    await vi.waitFor(() => expect(chrome.tabs.remove).toHaveBeenCalledExactlyOnceWith(8));
  });

  it("retries on a fresh tab when the reused tab crashed and rejects injection", async () => {
    // A crashed renderer keeps the tab's URL, so the query still matches it,
    // but Chrome refuses executeScript. The worker must move on to a new tab.
    chrome.tabs.query.mockResolvedValue([{ id: 7 }]);
    chrome.tabs.get.mockImplementation(async (id: number) => ({ id, status: "complete" }));
    chrome.tabs.create.mockResolvedValue({ id: 8 });
    chrome.scripting.executeScript.mockImplementation(async (options) => {
      if (options.files) {
        if (options.target.tabId === 7)
          throw new Error("Cannot access contents of the page. Extension manifest must request permission to access the respective host.");
        return [{ result: true }];
      }
      return [{ result: { status: 200, body: "chats" } }];
    });
    expect(await request()).toEqual({ id: "a", version: 3, status: 200, body: "chats" });
    expect(chrome.tabs.create).toHaveBeenCalledOnce();
    await vi.waitFor(() => expect(chrome.tabs.remove).toHaveBeenCalledExactlyOnceWith(8));
  });

  it("sends a platform's headers with the page's cookies", async () => {
    const pageFetchSource = readFileSync("chrome-extension/page_fetch.js", "utf8");
    const siteFetch = vi.fn(async () => new Response('{"conversations":[]}'));
    chrome.scripting.executeScript.mockImplementation(async (options) => {
      if (options.files) {
        // Run the injected page_fetch.js against the page's fetch.
        new Function("fetch", pageFetchSource)(siteFetch);
        return [];
      }
      return [{ result: (await options.func(...(options.args ?? []))) ?? null }];
    });
    const path = "/rest/app-chat/conversations?pageSize=100";
    message({ id: "a", platform: "grok-web", path, version: 3 });
    await vi.waitFor(() => expect(port.postMessage).toHaveBeenCalledOnce());
    expect(port.postMessage).toHaveBeenCalledWith({
      id: "a",
      version: 3,
      status: 200,
      body: '{"conversations":[]}',
      retryAfter: undefined,
    });
    expect(siteFetch).toHaveBeenCalledExactlyOnceWith(
      `https://grok.com${path}`,
      expect.objectContaining({
        method: "GET",
        credentials: "include",
        redirect: "error",
        headers: { Accept: "application/json" },
      }),
    );
  });

  it("waits for the Claude tab to load before injecting the reader", async () => {
    chrome.tabs.get.mockResolvedValue({ id: 7, status: "loading" });
    const pending = request();
    await vi.waitFor(() => expect(chrome.tabs.onUpdated.addListener).toHaveBeenCalledOnce());
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
    const updated = chrome.tabs.onUpdated.addListener.mock.calls[0]![0];
    updated(9, { status: "complete" });
    expect(chrome.scripting.executeScript).not.toHaveBeenCalled();
    updated(7, { status: "complete" });
    expect(await pending).toEqual({ id: "a", version: 3, status: 200, body: "chats" });
    expect(chrome.tabs.onUpdated.removeListener).toHaveBeenCalledWith(updated);
  });

  it("replaces an oversized serialized reply with 413", async () => {
    chrome.scripting.executeScript
      .mockResolvedValueOnce([{ result: true }])
      .mockResolvedValueOnce([{ result: { status: 200, body: '"'.repeat(32 * 1024 * 1024) } }]);
    expect(await request()).toEqual({ id: "a", version: 3, status: 413 });
  });

  it.each([null, {}, { status: "200" }])(
    "rejects a fetch reply without numeric status: %s",
    async (result) => {
      chrome.scripting.executeScript
        .mockResolvedValueOnce([{ result: true }])
        .mockResolvedValueOnce([{ result }]);
      expect(await request()).toEqual({
        id: "a",
        version: 3,
        status: 0,
        error: "The page changed during Sync; try Sync again",
      });
    },
  );

  it("reconnects five seconds after disconnect without duplicating startup connections", async () => {
    vi.useFakeTimers();
    chrome.runtime.onStartup.addListener.mock.calls[0]![0]();
    chrome.runtime.onInstalled.addListener.mock.calls[0]![0]();
    expect(chrome.runtime.connectNative).toHaveBeenCalledOnce();
    disconnect();
    await vi.advanceTimersByTimeAsync(4999);
    expect(chrome.runtime.connectNative).toHaveBeenCalledOnce();
    await vi.advanceTimersByTimeAsync(1);
    expect(chrome.runtime.connectNative).toHaveBeenCalledTimes(2);
  });
});
